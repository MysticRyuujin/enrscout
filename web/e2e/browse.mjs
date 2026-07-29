import { chromium } from "playwright";
import { mkdirSync } from "node:fs";

const BASE = process.env.WEB_BASE || "http://localhost:8081";
const OUT = new URL("./screenshots/", import.meta.url).pathname;
const NETWORKS = (process.env.WEB_NETWORKS || "mainnet,hoodi,sepolia")
  .split(",")
  .map((network) => network.trim())
  .filter(Boolean);

mkdirSync(OUT, { recursive: true });

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, ok: !!cond, detail });
  console.log(
    `${cond ? "PASS" : "FAIL"}  ${name}${detail ? ` - ${detail}` : ""}`,
  );
}

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1440, height: 1100 },
});
const page = await ctx.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));

async function goto(path, network) {
  const url = network
    ? `${BASE}${path}${path.includes("?") ? "&" : "?"}network=${network}`
    : `${BASE}${path}`;
  await page.goto(url, {
    waitUntil: "networkidle",
    timeout: 30000,
  });
  await page.waitForTimeout(5000);
}

for (const net of NETWORKS) {
  await goto("/", net);
  const tiles = await page.locator(".tile").count();
  const totalTxt =
    (await page.locator(".tile .tile-num").first().textContent()) || "0";
  const total = parseInt(totalTxt.replace(/,/g, ""), 10) || 0;
  const donuts = await page.locator(".donut").count();
  const mapCanvas = await page.locator(".map canvas").count();
  const tileLabels = await page.locator(".tile .tile-lbl").allTextContents();
  const statsResponse = await page.request.get(
    `${BASE}/api/v1/stats?network=${net}`,
  );
  const warmingUp =
    statsResponse.ok() && (await statsResponse.json()).warming_up === true;
  const warmupBanners = await page.locator(".warmup-banner").count();
  await page.screenshot({ path: `${OUT}${net}-overview.png` });
  check(
    `[${net}] overview: network is shareable in URL`,
    new URL(page.url()).searchParams.get("network") === net,
  );
  check(`[${net}] overview: >=6 stat tiles`, tiles >= 6, `${tiles}`);
  check(`[${net}] overview: total identities > 0`, total > 0, `${total}`);
  check(
    `[${net}] overview: identity and layer labels are explicit`,
    ["total identities", "execution identities", "consensus identities"].every(
      (label) => tileLabels.includes(label),
    ),
  );
  check(`[${net}] overview: donuts rendered`, donuts >= 2, `${donuts}`);
  check(`[${net}] overview: map canvas present`, mapCanvas >= 1);
  check(
    `[${net}] overview: warm-up banner matches API state`,
    warmupBanners === (warmingUp ? 1 : 0),
    `${warmupBanners}`,
  );
  if (warmingUp) {
    check(
      `[${net}] overview: warm-up disclosure is explicit`,
      await page
        .locator(".warmup-banner")
        .getByText(/client shares may still shift/i)
        .isVisible(),
    );
  }
  const methodology = page.locator(".disclaimer-banner");
  check(
    `[${net}] overview: methodology disclosure present`,
    (await methodology.count()) >= 1,
    `${await methodology.count()}`,
  );
  check(
    `[${net}] overview: methodology remains readable`,
    parseFloat(
      await methodology
        .locator("span")
        .first()
        .evaluate((el) => getComputedStyle(el).fontSize),
    ) >= 12.5,
  );

  await goto("/nodes", net);
  const rows = await page.locator(".nodes-table tbody tr").count();
  const filters = await page.locator(".filters select").count();
  const heading = (await page.locator(".page-head h1").textContent()) || "";
  await page.screenshot({ path: `${OUT}${net}-nodes.png` });
  check(`[${net}] nodes: table has rows`, rows > 0, `${rows}`);
  check(`[${net}] nodes: filter selects present`, filters >= 4, `${filters}`);
  check(
    `[${net}] nodes: heading describes identities`,
    /identities/i.test(heading),
    heading,
  );

  if (net === NETWORKS[0]) {
    if (NETWORKS.length > 1) {
      const switched = NETWORKS[1];
      await page.getByRole("button", { name: switched, exact: true }).click();
      await page.waitForFunction(
        (network) =>
          new URL(location.href).searchParams.get("network") === network,
        switched,
      );
      check(
        "network switch: updates the shareable URL",
        new URL(page.url()).searchParams.get("network") === switched,
      );
      await page.getByRole("button", { name: net, exact: true }).click();
      await page.waitForFunction(
        (network) =>
          new URL(location.href).searchParams.get("network") === network,
        net,
      );
      await page.waitForSelector(".nodes-table tbody tr");
    }
    check(
      "nodes: client and last-seen headers are sortable",
      (await page.locator(".sort-header").count()) === 2,
    );
    const headers = await page.locator(".nodes-table th").allTextContents();
    check(
      "nodes: redundant network and score columns are absent",
      !headers.includes("NET") &&
        !headers.some((header) => header.startsWith("SCORE")),
    );

    const clientFilter = page.locator('input[placeholder="client contains…"]');
    await clientFilter.fill("eth");
    await page.waitForTimeout(500);
    check(
      "nodes: partial filter applies without Enter",
      new URL(page.url()).searchParams.get("client") === "eth",
    );
    await clientFilter.fill("");
    await page.waitForTimeout(100);
    check(
      "nodes: clearing a filter applies immediately",
      !new URL(page.url()).searchParams.has("client"),
    );

    const ipFilter = page.locator('input[placeholder="IP address"]');
    check(
      "nodes: dedicated IP filter is present",
      (await ipFilter.count()) === 1,
    );
    await ipFilter.fill("127.");
    await page.waitForTimeout(500);
    check(
      "nodes: IP filter applies without Enter",
      new URL(page.url()).searchParams.get("ip") === "127.",
    );
    await ipFilter.fill("");
    await page.waitForTimeout(100);

    const clientSort = page.getByRole("button", { name: /Client/ });
    await clientSort.click();
    // Router navigation runs in a transition, so the header re-renders a tick after the URL.
    await page
      .waitForFunction(
        () => document.querySelector('th[aria-sort="ascending"]') !== null,
        null,
        { timeout: 2000 },
      )
      .catch(() => {});
    check(
      "nodes: client header defaults ascending",
      (await clientSort.locator("xpath=..").getAttribute("aria-sort")) ===
        "ascending",
    );
    await clientSort.click();
    check(
      "nodes: sort direction can be inverted",
      new URL(page.url()).searchParams.get("order") === "desc",
    );
  }

  const firstLink = page.locator(".nodes-table tbody tr td a").first();
  if ((await firstLink.count()) > 0) {
    await firstLink.click();
    await page.waitForTimeout(2500);
    const kv = await page.locator(".kv").count();
    const recordGap = await page
      .locator(".records-card .kv")
      .first()
      .evaluate((row) => {
        const label = row.querySelector(".k").getBoundingClientRect();
        const value = row.querySelector(".v").getBoundingClientRect();
        return Math.round(value.x - label.x);
      });
    await page.screenshot({ path: `${OUT}${net}-detail.png` });
    check(`[${net}] detail: field rows present`, kv > 5, `${kv}`);
    check(
      `[${net}] detail: record labels use compact columns`,
      recordGap <= 96,
      `${recordGap}px`,
    );
  }
}

await goto("/about");
const aboutText = (await page.locator(".prose").textContent()) || "";
await page.screenshot({ path: `${OUT}about.png` });
check("about: mentions MaxMind attribution", aboutText.includes("MaxMind"));

check(
  "no uncaught page errors",
  errors.length === 0,
  errors.slice(0, 3).join(" | "),
);

await browser.close();

const failed = results.filter((r) => !r.ok);
console.log(
  `\n${results.length - failed.length}/${results.length} checks passed`,
);
process.exit(failed.length === 0 ? 0 : 1);
