import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Link } from "react-router";
import { fetchForks } from "../api";
import { useNetwork } from "../network";
import {
  forkMode,
  layerForkMode,
  legendStates,
  stateHint,
  stateLabel,
} from "../readiness";
import type { ForkMode } from "../readiness";
import StatTiles from "../components/StatTiles";
import Notices from "../components/Notices";
import ReadinessTrend, {
  fmtTime,
  MAX_LABELED_SERIES,
  readyPool,
  readyShare,
  scheduledShare,
  TREND_COLOR,
} from "../components/ReadinessTrend";
import type { TrendSeries } from "../components/ReadinessTrend";
import {
  CATEGORICAL,
  clientColor,
  durationAgo,
  layerName,
  networkColor,
  nodesPath,
  num,
  OTHER_COLOR,
  whileVisible,
} from "../theme";
import type {
  ClientReadiness,
  ClientRelease,
  ForkReadiness,
  LayerReadiness,
  Readiness,
  ReadinessCounts,
  ReadinessPoint,
  ScheduledFork,
} from "../types";

const REFRESH_MS = 60_000;
// The zoomed trend spans the time since activation and as much before it, so the cutover sits
// mid-chart instead of in the last few pixels of a month of history.
const ZOOM_MIN_BEFORE_S = 2 * 3600;
const ZOOM_DEFAULT_FOR_S = 3 * 86400;

type TrendRange = "fork" | "all";
type TrendView = "overall" | "el" | "cl";
const MAX_CLIENT_LINES = 8;

function clientsAt(p: ReadinessPoint, layer: "el" | "cl") {
  return layer === "el" ? p.el_clients : p.cl_clients;
}

// Every client keeps its own color, so the tail folds into a neutral Other line, not a ninth hue.
function clientTrendSeries(
  points: ReadinessPoint[],
  layer: "el" | "cl",
): TrendSeries[] {
  const size = new Map<string, number>();
  for (const p of points)
    for (const [client, c] of Object.entries(clientsAt(p, layer) ?? {}))
      size.set(client, Math.max(size.get(client) ?? 0, readyPool(c)));
  const ranked = [...size]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([client]) => client);
  const shown =
    ranked.length > MAX_CLIENT_LINES
      ? ranked.slice(0, MAX_CLIENT_LINES - 1)
      : ranked;
  const rest = ranked.slice(shown.length);
  const series: TrendSeries[] = shown.map((client) => ({
    key: client,
    short: client,
    name: client,
    color: clientColor(client),
    counts: points.map((p) => clientsAt(p, layer)?.[client]),
  }));
  if (rest.length)
    series.push({
      key: "Other",
      short: "Other",
      name: `Other (${rest.length} clients)`,
      color: OTHER_COLOR,
      counts: points.map((p) => {
        const all = clientsAt(p, layer);
        if (!all) return undefined;
        const sum: Partial<ReadinessCounts> = {};
        for (const client of rest)
          for (const [state, n] of Object.entries(all[client] ?? {}))
            sum[state as Readiness] = (sum[state as Readiness] ?? 0) + (n ?? 0);
        return sum;
      }),
    });
  return series;
}
const STATES: Readiness[] = [
  "ready",
  "pending",
  "not_ready",
  "mismatch",
  "unknown",
  "stale",
];

// Validated as a set against the dark panel surface; unknown and stale are deliberately neutral.
// Pending is the ready hue hatched: the node has the fork, but nobody has seen it run it yet.
const STATE_COLOR: Record<Readiness, string> = {
  ready: CATEGORICAL[1],
  pending: `repeating-linear-gradient(135deg, ${CATEGORICAL[1]} 0 3px, ${CATEGORICAL[1]}4d 3px 6px)`,
  not_ready: CATEGORICAL[5],
  mismatch: CATEGORICAL[4],
  unknown: OTHER_COLOR,
  stale: "#2a3547",
};

function remaining(s: number): string {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d ${h}h` : `${h}h ${m}m`;
}

function countdown(target: number, now: number): string {
  const s = target - now;
  return s > 0
    ? `activates in ${remaining(s)}`
    : `activated ${durationAgo(-s)}`;
}

function cancelledCountdown(target: number, now: number): string {
  const s = target - now;
  return s > 0
    ? `would have activated in ${remaining(s)}`
    : `would have activated ${durationAgo(-s)}`;
}

function utc(unix: number): string {
  return (
    new Date(unix * 1000).toISOString().replace("T", " ").slice(0, 16) + " UTC"
  );
}

function pct(n: number, of: number): string {
  return of ? `${((n / of) * 100).toFixed(1)}%` : "-";
}

function StackedBar({
  counts,
  mode,
  total,
}: {
  counts: ReadinessCounts;
  mode: ForkMode;
  total: number;
}) {
  const present = STATES.filter((s) => counts[s] > 0);
  return (
    <span
      className="rd-bar"
      role="img"
      aria-label={present
        .map((s) => `${num(counts[s])} ${stateLabel(s, mode)}`)
        .join(", ")}
    >
      {present.map((s) => (
        <span
          key={s}
          className="rd-seg"
          style={{ flexGrow: counts[s], background: STATE_COLOR[s] }}
          title={`${stateLabel(s, mode)}: ${num(counts[s])} (${pct(counts[s], total)})`}
        />
      ))}
    </span>
  );
}

function Legend({ layer, mode }: { layer: "el" | "cl"; mode: ForkMode }) {
  return (
    <div className="rd-legend">
      {legendStates(layer, mode).map((s) => (
        <span key={s} title={stateHint(s, mode)}>
          <span className="swatch" style={{ background: STATE_COLOR[s] }} />
          {stateLabel(s, mode)}
        </span>
      ))}
      <Link to="/about#fork-readiness">What these mean</Link>
    </div>
  );
}

function LineKey({ dashed }: { dashed?: boolean }) {
  return (
    <svg width="18" height="8" aria-hidden="true">
      <line
        x1="0"
        x2="18"
        y1="4"
        y2="4"
        stroke="currentColor"
        strokeWidth="2"
        strokeDasharray={dashed ? "4 3" : undefined}
      />
    </svg>
  );
}

function hasRelease(r?: ClientRelease, mode?: ForkMode): boolean {
  if (mode === "cancelled") return !!r?.unscheduled_in?.length && !r.outdated;
  return !!r?.min_versions?.length && !r.outdated;
}

function releaseText(r?: ClientRelease, mode?: ForkMode): string {
  if (!r) return "not tracked";
  if (r.outdated) return "table outdated for this fork date";
  if (mode === "cancelled") {
    if (!r.min_versions?.length)
      return r.prerelease
        ? `affected pre-release ${r.prerelease}`
        : "no affected release";
    const fixed = r.unscheduled_in?.length
      ? ` · fixed in ${r.unscheduled_in.join(" / ")}`
      : " · no fix yet";
    return `affected from ${r.min_versions.join(" / ")}${fixed}`;
  }
  if (r.min_versions?.length) return `✓ ${r.min_versions.join(" / ")}`;
  if (r.prerelease) return `pre-release ${r.prerelease}`;
  return "no release yet";
}

function ReleaseLink({ text, url }: { text: string; url?: string }) {
  return url ? (
    <a href={url} target="_blank" rel="noreferrer">
      {text}
    </a>
  ) : (
    <>{text}</>
  );
}

function LatestRelease({ r }: { r: ClientRelease }) {
  if (!hasRelease(r)) return <>-</>;
  if (r.latest) return <ReleaseLink text={r.latest} url={r.latest_url} />;
  if (r.min_versions!.length === 1)
    return <ReleaseLink text={r.min_versions![0]} url={r.url} />;
  return <>-</>;
}

const VERSION_BADGE: Record<string, string> = {
  meets: "release ≥ minimum",
  below: "older release",
  dev_build: "dev / rc build",
  mixed: "mixed builds",
  affected: "schedules the cancelled fork",
  unaffected: "does not schedule it",
};

function ForkSchedule({
  network,
  schedule,
  now,
}: {
  network: string;
  schedule?: ScheduledFork[];
  now: number;
}) {
  if (!schedule?.length) return null;
  return (
    <div className="card">
      <h3>Fork schedule</h3>
      <p className="card-subtitle">
        Every upgrade on <span className="net-name">{network}</span> that
        activates at a set time after genesis. Execution and consensus forks
        that activate at the same time share a row.
      </p>
      <div className="table-wrap">
        <table className="nodes-table">
          <thead>
            <tr>
              <th>Upgrade</th>
              <th>Execution</th>
              <th>Consensus</th>
              <th>Activation</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {schedule.map((f) => (
              <tr key={`${f.time}-${f.name}`}>
                <td>
                  {f.cancelled ? (
                    <s>{f.name}</s>
                  ) : f.tracked ? (
                    <strong>{f.name}</strong>
                  ) : (
                    f.name
                  )}
                </td>
                <td>{f.el ?? "-"}</td>
                <td>
                  {f.cl ? `${f.cl} at epoch ${num(f.cl_epoch ?? 0)}` : "-"}
                </td>
                <td>{utc(f.time)}</td>
                <td>
                  {f.cancelled
                    ? `cancelled · ${cancelledCountdown(f.time, now)}`
                    : countdown(f.time, now)}
                  {f.tracked && " · tracked"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function nodesLink(layer: "el" | "cl", params: Record<string, string>) {
  return {
    pathname: nodesPath(layer),
    search: new URLSearchParams({ ...params, fork: "all" }).toString(),
  };
}

function ReadinessCells({
  counts,
  total,
  mode,
  ready,
}: {
  counts: ReadinessCounts;
  total: number;
  mode: ForkMode;
  ready?: ReactNode;
}) {
  return (
    <>
      <td className="rd-bar-cell">
        <StackedBar counts={counts} mode={mode} total={total} />
      </td>
      <td className="num">
        {ready ?? num(counts.ready)} / {num(total)}
      </td>
      <td className="num">{pct(counts.ready, total)}</td>
    </>
  );
}

function ClientRow({
  layer,
  c,
  release,
  mode,
}: {
  layer: "el" | "cl";
  c: ClientReadiness;
  release?: ClientRelease;
  mode: ForkMode;
}) {
  const { network } = useNetwork();
  const [open, setOpen] = useState(false);
  return (
    <>
      <tr>
        <td>
          <button
            className="rd-expand"
            onClick={() => setOpen(!open)}
            aria-expanded={open}
          >
            {open ? "▾" : "▸"} {c.client}
          </button>
        </td>
        <td className={hasRelease(release, mode) ? "rd-rel ok" : "rd-rel"}>
          {releaseText(release, mode)}
        </td>
        <ReadinessCells
          counts={c.counts}
          total={c.total}
          mode={mode}
          ready={
            <Link
              to={nodesLink(layer, {
                network,
                client: c.client,
                client_exact: "yes",
                identified: "recent",
                readiness: "ready",
              })}
            >
              {num(c.counts.ready)}
            </Link>
          }
        />
      </tr>
      {open &&
        c.versions.map((v) => (
          <tr key={v.version} className="rd-version">
            <td className="mono">{v.version}</td>
            <td>{v.release ? (VERSION_BADGE[v.release] ?? "") : ""}</td>
            <ReadinessCells counts={v.counts} total={v.total} mode={mode} />
          </tr>
        ))}
    </>
  );
}

function LayerCard({
  layer,
  data,
  releases,
  mode,
}: {
  layer: "el" | "cl";
  data: LayerReadiness;
  releases: ClientRelease[];
  mode: ForkMode;
}) {
  const releaseOf = new Map(
    releases.filter((r) => r.layer === layer).map((r) => [r.client, r]),
  );
  return (
    <div className="card">
      <h3>{layerName(layer)} clients</h3>
      <p className="card-subtitle">
        {num(data.total)} identities. Client rows count recognized clients with
        a verified handshake in the last 7 days. Ready counts link to the
        matching nodes.
      </p>
      <Legend layer={layer} mode={mode} />
      <div className="table-wrap">
        <table className="nodes-table">
          <thead>
            <tr>
              <th>Client</th>
              <th>
                {mode === "cancelled"
                  ? "Affected releases"
                  : "Release with the fork"}
              </th>
              <th>Readiness</th>
              <th className="num">
                {mode === "activated"
                  ? "Upgraded"
                  : mode === "cancelled"
                    ? "Unscheduled"
                    : "Ready"}
              </th>
              <th className="num">Share</th>
            </tr>
          </thead>
          <tbody>
            {data.clients.map((c) => (
              <ClientRow
                key={c.client}
                layer={layer}
                c={c}
                release={releaseOf.get(c.client)}
                mode={mode}
              />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export default function Forks() {
  const { network } = useNetwork();
  const [data, setData] = useState<ForkReadiness | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now() / 1000);
  const [range, setRange] = useState<TrendRange | null>(null);
  const [view, setView] = useState<TrendView>("overall");
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set());

  useEffect(() => {
    let live = true;
    setData(null);
    setErr(null);
    const load = () =>
      fetchForks(network)
        .then((d) => live && (setData(d), setErr(null)))
        .catch(
          (e) => live && setErr(e instanceof Error ? e.message : String(e)),
        );
    void load();
    const tick = whileVisible(load);
    const timer = window.setInterval(() => {
      setNow(Date.now() / 1000);
      tick();
    }, REFRESH_MS);
    return () => {
      live = false;
      window.clearInterval(timer);
    };
  }, [network]);

  const color = networkColor(network);
  if (err && !data)
    return (
      <div className="page">
        <div className="error">API unreachable: {err}</div>
      </div>
    );
  if (!data)
    return (
      <div className="page">
        <div className="loading">Loading…</div>
      </div>
    );

  if (data.phase === "none") {
    return (
      <div className="page forks">
        <div className="page-head">
          <h1>
            <span className="net-dot" style={{ background: color }} /> Fork
            readiness
          </h1>
        </div>
        <Notices notices={data.notices} />
        <div className="card">
          <p className="empty">
            No fork is scheduled on <span className="net-name">{network}</span>.
            The tracker follows the next execution and consensus fork in the
            crawler&apos;s schedule for the network, and keeps a fork for 14
            days after it activates unless a later fork is scheduled.
          </p>
        </div>
        <ForkSchedule network={network} schedule={data.schedule} now={now} />
      </div>
    );
  }

  const activated = data.phase === "activated";
  const mode = forkMode(data.phase);
  const cancelled = mode === "cancelled";
  const activation =
    data.fork.el?.time ??
    (data.fork.cl ? Date.parse(data.fork.cl.time) / 1000 : undefined);
  const passed = cancelled && activation !== undefined && now >= activation;
  const layers = (["el", "cl"] as const).filter((l) => data.layers[l]);
  const tiles = layers.flatMap((l) => {
    const m = layerForkMode(data, l);
    const c = data.layers[l]!.counts;
    const pool = readyPool(c);
    const share = readyShare(c);
    return [
      {
        label: `${l.toUpperCase()} ${m === "activated" ? "upgraded" : m === "cancelled" ? "unscheduled" : "ready"}`,
        value: c.ready,
        hint:
          share === null
            ? undefined
            : `${(share * 100).toFixed(1)}% of ${num(pool)}`,
      },
      ...(c.pending
        ? [
            {
              label: `${l.toUpperCase()} pending`,
              value: c.pending,
              hint: `${pct(c.pending, pool)}, fork scheduled`,
            },
          ]
        : []),
      {
        label: `${l.toUpperCase()} ${m === "activated" ? "left behind" : m === "cancelled" ? (passed ? "on cancelled fork" : "still scheduled") : "not scheduled"}`,
        value: c.not_ready,
        hint: pct(c.not_ready, pool),
      },
    ];
  });
  const mismatch = layers.reduce(
    (a, l) => a + data.layers[l]!.counts.mismatch,
    0,
  );
  if (mismatch)
    tiles.push({
      label: "other schedule",
      value: mismatch,
      hint: "wrong or overridden fork time",
    });
  const clUnknown = data.layers.cl?.counts.unknown ?? 0;
  if (clUnknown)
    tiles.push({
      label: "CL schedule unknown",
      value: clUnknown,
      hint: "no ENR eth2 entry",
    });
  const lagging = data.layers.el?.sync.lagging ?? 0;
  if (data.layers.el)
    tiles.push({
      label: "EL lagging",
      value: lagging,
      hint: "behind the observed tip",
    });
  const snapshotAt = data.snapshot_generated_at
    ? Date.parse(data.snapshot_generated_at) / 1000
    : undefined;
  const preActivationSnapshot =
    activated &&
    activation !== undefined &&
    snapshotAt !== undefined &&
    snapshotAt < activation;
  const history = data.history?.points ?? [];
  const zoomable = activated && activation !== undefined;
  const trendRange: TrendRange =
    range ??
    (zoomable && now - activation < ZOOM_DEFAULT_FOR_S ? "fork" : "all");
  const trendPoints =
    zoomable && trendRange === "fork"
      ? history.filter(
          (p) =>
            p.at >= activation - Math.max(now - activation, ZOOM_MIN_BEFORE_S),
        )
      : history;
  const clientViews = layers.filter((l) =>
    history.some((p) => clientsAt(p, l)),
  );
  const trendView: TrendView =
    view !== "overall" && clientViews.includes(view) ? view : "overall";
  const allSeries: TrendSeries[] =
    trendView === "overall"
      ? layers.map((l) => ({
          key: l,
          short: l.toUpperCase(),
          name: layerName(l),
          color: TREND_COLOR[l],
          counts: trendPoints.map((p) => (l === "el" ? p.el : p.cl)),
        }))
      : clientTrendSeries(trendPoints, trendView);
  const trendSeries = allSeries.filter((s) => !hidden.has(s.key));
  const clientStart =
    trendView === "overall"
      ? -1
      : trendPoints.findIndex((p) => clientsAt(p, trendView));
  const chooseView = (v: TrendView) => {
    setView(v);
    setHidden(new Set());
  };
  const toggleSeries = (key: string) =>
    setHidden((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  const releases = data.releases;
  const released = releases.filter((r) => hasRelease(r)).length;
  if (releases.length && mode === "scheduled")
    tiles.push({
      label: "clients with a release",
      value: released,
      hint: `of ${releases.length} tracked`,
    });

  return (
    <div className="page forks">
      <div className="page-head">
        <h1>
          <span className="net-dot" style={{ background: color }} />{" "}
          {data.fork.name} on {network}
          <span className={`phase-badge ${mode}`}>{data.phase}</span>
        </h1>
        <p className="sub">
          {activation && (
            <>
              <strong>
                {cancelled
                  ? cancelledCountdown(activation, now)
                  : countdown(activation, now)}
              </strong>{" "}
              · {utc(activation)}
            </>
          )}
          {data.fork.el && (
            <>
              {" "}
              · EL {data.fork.el.name} at timestamp {data.fork.el.time}
            </>
          )}
          {data.fork.cl && (
            <>
              {" "}
              · CL {data.fork.cl.name} at epoch {num(data.fork.cl.epoch)}
            </>
          )}
          {data.snapshot_generated_at && (
            <>
              {" "}
              · updated{" "}
              {durationAgo(now - Date.parse(data.snapshot_generated_at) / 1000)}
            </>
          )}
        </p>
      </div>

      <Notices notices={data.notices} />
      {err && <div className="error">API unreachable: {err}</div>}
      {preActivationSnapshot && (
        <div className="error">
          The latest snapshot is from before activation, so no node can show as
          upgraded yet. Counts update with the next snapshot.
        </div>
      )}

      <section className="disclaimer-banner" aria-label="Methodology">
        <span>
          {cancelled
            ? "This fork is cancelled. Unscheduled means the identity no longer advertises it: the execution fork ID Next is not the cancelled time, or the consensus record does not name the cancelled version and epoch. Still scheduled means it does, and the node forks away from the network at the time the fork would have activated."
            : activated
              ? "Upgraded means the identity advertises the new fork. Pending means it scheduled the fork, but the crawler has not seen it on the fork yet. Left behind means it did not advertise the fork."
              : "Ready means the identity itself advertises the fork: the execution fork ID names the fork time, or the consensus record names the fork version and epoch."}{" "}
          This is the node&apos;s own schedule, not its version string.
          {releases.length > 0 && (
            <>
              {" "}
              Release labels come from an operator-maintained release file
              {data.releases_updated && <>, updated {data.releases_updated}</>}.
            </>
          )}{" "}
          <Link to="/about#fork-readiness">Methodology</Link>
        </span>
      </section>

      <StatTiles tiles={tiles} />

      <div className="card">
        <h3>Adoption over time</h3>
        <p className="card-subtitle">
          {cancelled
            ? "Share of identities that have the fork in their schedule. It rose while the fork was scheduled and falls as nodes drop it"
            : "Share of identities that have the fork in their schedule"}
          {trendView === "overall"
            ? "."
            : `, per ${layerName(trendView).toLowerCase()} client with a fresh fingerprint.`}
        </p>
        {(clientViews.length > 0 || zoomable) && (
          <div className="trend-controls">
            {clientViews.length > 0 && (
              <div className="trend-range" role="group" aria-label="Series">
                {(["overall", ...clientViews] as const).map((v) => (
                  <button
                    key={v}
                    className={trendView === v ? "active" : undefined}
                    aria-pressed={trendView === v}
                    onClick={() => chooseView(v)}
                  >
                    {v === "overall" ? "Overall" : `${v.toUpperCase()} clients`}
                  </button>
                ))}
              </div>
            )}
            {zoomable && (
              <div className="trend-range" role="group" aria-label="Time range">
                {(
                  [
                    ["fork", "Around activation"],
                    ["all", "All history"],
                  ] as const
                ).map(([r, label]) => (
                  <button
                    key={r}
                    className={trendRange === r ? "active" : undefined}
                    aria-pressed={trendRange === r}
                    onClick={() => setRange(r)}
                  >
                    {label}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
        <div className="rd-legend">
          {trendView === "overall" ? (
            <>
              {layers.map((l) => (
                <span key={l}>
                  <span
                    className="swatch"
                    style={{ background: TREND_COLOR[l] }}
                  />
                  {layerName(l)}
                </span>
              ))}
              {activated && (
                <>
                  <span>
                    <LineKey /> scheduled or upgraded
                  </span>
                  <span>
                    <LineKey dashed /> seen on the fork
                  </span>
                </>
              )}
            </>
          ) : (
            allSeries.map((s) => {
              const last = [...s.counts]
                .reverse()
                .find((c) => c && readyPool(c));
              const share = scheduledShare(last);
              return (
                <button
                  key={s.key}
                  className="trend-chip"
                  aria-pressed={!hidden.has(s.key)}
                  onClick={() => toggleSeries(s.key)}
                >
                  <span className="swatch" style={{ background: s.color }} />
                  {s.name}
                  {share !== null && (
                    <span className="trend-chip-value">
                      {(share * 100).toFixed(1)}%
                    </span>
                  )}
                </button>
              );
            })
          )}
        </div>
        <ReadinessTrend
          points={trendPoints}
          activation={activation}
          cancelled={cancelled}
          series={trendSeries}
          seenLines={trendView === "overall"}
        />
        {clientStart > 0 && (
          <p className="trend-note">
            Per-client history starts {fmtTime(trendPoints[clientStart].at)}.
          </p>
        )}
        {trendView !== "overall" && trendSeries.length > MAX_LABELED_SERIES && (
          <p className="trend-note">
            Select up to {MAX_LABELED_SERIES} clients in the legend to label
            their lines.
          </p>
        )}
      </div>

      {layers.map((l) => (
        <LayerCard
          key={l}
          layer={l}
          data={data.layers[l]!}
          releases={releases}
          mode={layerForkMode(data, l)}
        />
      ))}

      {releases.length > 0 && (
        <div className="card">
          <h3>Client releases</h3>
          <p className="card-subtitle">
            {cancelled ? (
              <>
                Releases of each client that schedule the cancelled{" "}
                {data.fork.name} for <span className="net-name">{network}</span>
                : versions from the first release up to the fixed release fork
                away. Older releases never had the schedule.
              </>
            ) : (
              <>
                First release of each client that ships the {data.fork.name}{" "}
                schedule for <span className="net-name">{network}</span>, and
                the latest release to run, which can carry later fixes.
              </>
            )}
            {data.releases_updated && <> Updated {data.releases_updated}.</>}
          </p>
          <div className="table-wrap">
            <table className="nodes-table">
              <thead>
                <tr>
                  <th>Client</th>
                  <th>Layer</th>
                  <th>{cancelled ? "Schedules the fork from" : "Minimum"}</th>
                  <th>{cancelled ? "Fixed in" : "Latest"}</th>
                  <th>Minimum date</th>
                </tr>
              </thead>
              <tbody>
                {releases.map((r) => {
                  const rowCancelled =
                    r.layer === "el" || r.layer === "cl"
                      ? layerForkMode(data, r.layer) === "cancelled"
                      : cancelled;
                  return (
                    <tr key={r.layer + r.client}>
                      <td>{r.client}</td>
                      <td>{layerName(r.layer)}</td>
                      <td className={hasRelease(r) ? "rd-rel ok" : "rd-rel"}>
                        <ReleaseLink
                          text={
                            rowCancelled && !r.outdated
                              ? (r.min_versions?.join(" / ") ??
                                (r.prerelease
                                  ? `pre-release ${r.prerelease}`
                                  : "-"))
                              : releaseText(r)
                          }
                          url={r.url}
                        />
                      </td>
                      <td>
                        {rowCancelled ? (
                          r.outdated ? (
                            "-"
                          ) : (
                            <ReleaseLink
                              text={
                                r.unscheduled_in?.join(" / ") ?? "no fix yet"
                              }
                              url={
                                r.unscheduled_in?.length
                                  ? r.latest_url
                                  : undefined
                              }
                            />
                          )
                        ) : (
                          <LatestRelease r={r} />
                        )}
                      </td>
                      <td>{r.released ?? "-"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <ForkSchedule network={network} schedule={data.schedule} now={now} />
    </div>
  );
}
