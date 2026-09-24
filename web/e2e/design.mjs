// Design check: renders every page at the contract viewports, applies the mechanical rules in
// docs/design-contract.md, and diffs each screenshot against the last accepted baseline so the
// reviewer looks only at what changed. Local only; baselines live outside git.
//
//   node e2e/design.mjs            # check against baseline, write current + diff images
//   node e2e/design.mjs --update   # accept the current screenshots as the new baseline
import { chromium } from "playwright";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import pixelmatch from "pixelmatch";
import { PNG } from "pngjs";

const BASE = process.env.WEB_BASE || "http://localhost:8081";
const NETWORK = process.env.WEB_NETWORK || "sepolia";
const UPDATE = process.argv.includes("--update");
const ROOT = new URL("./design/", import.meta.url).pathname;
const DIRS = {
  current: `${ROOT}current/`,
  baseline: `${ROOT}baseline/`,
  diff: `${ROOT}diff/`,
};

// Heights only bound the first screen; screenshots are full page.
const VIEWPORTS = [
  { name: "phone", width: 390, height: 844 },
  { name: "tablet", width: 820, height: 1180 },
  { name: "laptop", width: 1280, height: 800 },
  { name: "desktop", width: 1920, height: 1080 },
];

// A pixel differs when any channel moves more than this share of its range. Below the threshold
// anti-aliasing noise would flag every run.
const PIXEL_THRESHOLD = 0.1;
// Screenshots whose changed-pixel share stays under this are reported as unchanged.
const CHANGED_SHARE = 0.001;

for (const dir of [DIRS.current, DIRS.diff]) {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
}
mkdirSync(DIRS.baseline, { recursive: true });

const results = [];
function report(level, name, ok, detail = "") {
  results.push({ level, name, ok, detail });
  const tag = ok ? "PASS" : level;
  console.log(`${tag.padEnd(4)}  ${name}${detail ? ` - ${detail}` : ""}`);
}
const fail = (name, ok, detail) => report("FAIL", name, ok, detail);
const warn = (name, ok, detail) => report("WARN", name, ok, detail);

const browser = await chromium.launch();
const ctx = await browser.newContext({ deviceScaleFactor: 1 });
const page = await ctx.newPage();
const pageErrors = [];
page.on("pageerror", (e) => pageErrors.push(e.message));

// Ages and countdowns are relative to the wall clock, so two runs an hour apart would differ
// without a code change. Pin the clock one minute past the snapshot the site is serving.
const meta = await (await page.request.get(`${BASE}/api/v1/meta`)).json();
await page.clock.setFixedTime(new Date(Date.parse(meta.generated_at) + 60_000));
await page.route(/\/api\/v1\/(stats|meta)(\?|$)/, async (route) => {
  const response = await route.fetch();
  const body = await response.json();
  for (const key of ["snapshot_age_seconds", "age_seconds"]) {
    if (key in body) body[key] = 60;
  }
  await route.fulfill({ response, json: body });
});

async function goto(path) {
  const url = `${BASE}${path}${path.includes("?") ? "&" : "?"}network=${NETWORK}`;
  await page.goto(url, { waitUntil: "networkidle", timeout: 30000 });
  await page.waitForTimeout(1500);
}

const RULES = {
  noHorizontalScroll() {
    const doc = document.documentElement;
    const limit = window.innerWidth + 1;
    if (doc.scrollWidth <= limit) return [];
    // Overflowing text does not widen its element's box, so measure the text range too.
    const right = (el) => {
      const range = document.createRange();
      range.selectNodeContents(el);
      return Math.max(
        el.getBoundingClientRect().right,
        range.getBoundingClientRect().right,
      );
    };
    const over = (el) => right(el) > limit;
    const culprits = [...document.querySelectorAll("body *")]
      .filter((el) => over(el) && ![...el.children].some(over))
      .slice(0, 3)
      .map(
        (el) =>
          `${el.tagName.toLowerCase()}.${el.className} ends at ${Math.round(right(el))}px`,
      );
    return [
      `page scrolls horizontally: ${doc.scrollWidth}px in a ${window.innerWidth}px viewport (${culprits.join("; ")})`,
    ];
  },
  // A select shows only its chosen option, so a label wider than the box is silently clipped.
  selectLabelsFit() {
    const canvas = document.createElement("canvas").getContext("2d");
    const out = [];
    for (const sel of document.querySelectorAll("select")) {
      if (!sel.offsetParent) continue;
      const cs = getComputedStyle(sel);
      canvas.font = `${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
      const label = sel.options[sel.selectedIndex]?.text ?? "";
      const text = canvas.measureText(label).width;
      const room =
        sel.clientWidth -
        parseFloat(cs.paddingLeft) -
        parseFloat(cs.paddingRight) -
        20;
      if (text > room)
        out.push(
          `"${label}" needs ${Math.ceil(text)}px, has ${Math.floor(room)}px`,
        );
    }
    return out;
  },
  gridsAreEven() {
    const out = [];
    for (const grid of document.querySelectorAll(".filters, .tiles")) {
      const kids = [...grid.children].filter((k) => k.offsetParent);
      if (kids.length < 2) continue;
      const widths = new Set(
        kids.map((k) => Math.round(k.getBoundingClientRect().width)),
      );
      if (widths.size > 1)
        out.push(
          `${grid.className}: ${widths.size} distinct widths (${[...widths].join(", ")})`,
        );
      const rows = new Map();
      for (const k of kids) {
        const r = k.getBoundingClientRect();
        const row = Math.round(r.top);
        rows.set(row, [...(rows.get(row) ?? []), Math.round(r.height)]);
      }
      for (const [row, heights] of rows) {
        if (new Set(heights).size > 1)
          out.push(
            `${grid.className}: row at ${row}px has heights ${heights.join(", ")}`,
          );
      }
    }
    return out;
  },
  // A lone control on the last row of a multi-row grid reads as a mistake. Reviewed, not failed:
  // an auto-fill grid cannot avoid it at every width.
  noOrphanRow() {
    const out = [];
    for (const grid of document.querySelectorAll(".filters, .tiles")) {
      const kids = [...grid.children].filter((k) => k.offsetParent);
      const tops = kids.map((k) => Math.round(k.getBoundingClientRect().top));
      const rows = [...new Set(tops)];
      if (rows.length < 2 || rows.length === kids.length) continue;
      const last = tops.filter((t) => t === rows[rows.length - 1]).length;
      if (last === 1)
        out.push(
          `${grid.className}: last of ${rows.length} rows holds one control`,
        );
    }
    return out;
  },
  readableText() {
    const out = new Set();
    for (const el of document.querySelectorAll("body *")) {
      if (!el.offsetParent || !el.childNodes.length) continue;
      const hasText = [...el.childNodes].some(
        (n) => n.nodeType === 3 && n.textContent.trim(),
      );
      if (!hasText) continue;
      const size = parseFloat(getComputedStyle(el).fontSize);
      if (size < 11)
        out.add(`${el.tagName.toLowerCase()}.${el.className}: ${size}px`);
    }
    return [...out].slice(0, 5);
  },
  tapTargets() {
    if (window.innerWidth > 500) return [];
    const out = new Set();
    for (const el of document.querySelectorAll("button, select, input")) {
      if (!el.offsetParent) continue;
      const h = el.getBoundingClientRect().height;
      if (h > 0 && h < 28)
        out.add(
          `${el.tagName.toLowerCase()}.${el.className} is ${Math.round(h)}px tall`,
        );
    }
    return [...out].slice(0, 5);
  },
};

async function applyRules(label) {
  for (const [rule, fn] of Object.entries(RULES)) {
    const violations = await page.evaluate(fn);
    const soft =
      rule === "noOrphanRow" ||
      rule === "readableText" ||
      rule === "tapTargets";
    (soft ? warn : fail)(
      `${label}: ${rule}`,
      violations.length === 0,
      violations.join("; "),
    );
  }
}

function compare(name) {
  const currentPath = `${DIRS.current}${name}.png`;
  const baselinePath = `${DIRS.baseline}${name}.png`;
  if (!existsSync(baselinePath)) return "new";
  const a = PNG.sync.read(readFileSync(currentPath));
  const b = PNG.sync.read(readFileSync(baselinePath));
  if (a.width !== b.width || a.height !== b.height) {
    return `size ${b.width}x${b.height} -> ${a.width}x${a.height}`;
  }
  const diff = new PNG({ width: a.width, height: a.height });
  const changed = pixelmatch(a.data, b.data, diff.data, a.width, a.height, {
    threshold: PIXEL_THRESHOLD,
  });
  const share = changed / (a.width * a.height);
  if (share < CHANGED_SHARE) return "same";
  writeFileSync(`${DIRS.diff}${name}.png`, PNG.sync.write(diff));
  return `${(share * 100).toFixed(2)}% of pixels`;
}

const changed = [];
async function shoot(name) {
  // The basemap draws tiles in a nondeterministic order, so it is masked out of the diff.
  await page.screenshot({
    path: `${DIRS.current}${name}.png`,
    fullPage: true,
    mask: [page.locator(".map canvas")],
  });
  const outcome = compare(name);
  if (outcome !== "same") changed.push(`${name}: ${outcome}`);
}

// The detail page needs a real node; take the first row of the execution table.
await page.setViewportSize({ width: 1280, height: 800 });
await goto("/nodes/execution");
const detailHref = await page
  .locator(".nodes-table tbody tr a")
  .first()
  .getAttribute("href");
const detailPath = detailHref ? new URL(detailHref, BASE).pathname : null;

const PAGES = [
  ["overview", "/"],
  ["nodes-el", "/nodes/execution"],
  ["nodes-cl", "/nodes/consensus"],
  ["forks", "/forks"],
  ["about", "/about"],
];
if (detailPath) PAGES.push(["detail", detailPath]);

for (const vp of VIEWPORTS) {
  await page.setViewportSize({ width: vp.width, height: vp.height });
  for (const [name, path] of PAGES) {
    const label = `[${vp.name} ${vp.width}] ${name}`;
    await goto(path);
    await applyRules(label);
    await shoot(`${vp.name}-${name}`);
  }
}

fail(
  "no page errors",
  pageErrors.length === 0,
  pageErrors.slice(0, 3).join(" | "),
);
await browser.close();

if (UPDATE) {
  for (const f of readdirSync(DIRS.current))
    copyFileSync(`${DIRS.current}${f}`, `${DIRS.baseline}${f}`);
  console.log(
    `\nBaseline updated: ${readdirSync(DIRS.current).length} screenshots`,
  );
} else if (changed.length) {
  console.log(
    `\nChanged screenshots (review each against docs/design-contract.md, then --update):`,
  );
  for (const c of changed) console.log(`  ${c}`);
  console.log(`  current: ${DIRS.current}\n  diff:    ${DIRS.diff}`);
} else {
  console.log("\nNo screenshot changed against the baseline.");
}

const failed = results.filter((r) => !r.ok && r.level === "FAIL");
const warned = results.filter((r) => !r.ok && r.level === "WARN");
console.log(
  `\n${results.length - failed.length - warned.length} passed, ${warned.length} warnings, ${failed.length} failed`,
);
process.exit(failed.length === 0 ? 0 : 1);
