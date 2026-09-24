import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Link } from "react-router";
import { fetchForks } from "../api";
import { useNetwork } from "../network";
import StatTiles from "../components/StatTiles";
import ReadinessTrend, {
  readyPool,
  readyShare,
  TREND_COLOR,
} from "../components/ReadinessTrend";
import {
  CATEGORICAL,
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
} from "../types";

const REFRESH_MS = 60_000;
const STATES: Readiness[] = [
  "ready",
  "not_ready",
  "mismatch",
  "unknown",
  "stale",
];

// Validated as a set against the dark panel surface; unknown and stale are deliberately neutral.
const STATE_COLOR: Record<Readiness, string> = {
  ready: CATEGORICAL[1],
  not_ready: CATEGORICAL[5],
  mismatch: CATEGORICAL[4],
  unknown: OTHER_COLOR,
  stale: "#2a3547",
};

function stateLabel(state: Readiness, activated: boolean): string {
  switch (state) {
    case "ready":
      return activated ? "upgraded" : "fork scheduled";
    case "not_ready":
      return activated ? "left behind" : "not scheduled";
    case "mismatch":
      return "other schedule";
    case "unknown":
      return "no schedule advertised";
    case "stale":
      return "older fork";
  }
}

// Only the states a layer's bars can draw: stale rows never reach a client row, execution rows are
// never unknown, and after activation a row is either upgraded or left behind.
function legendStates(layer: "el" | "cl", activated: boolean): Readiness[] {
  if (activated) return ["ready", "not_ready"];
  return layer === "el"
    ? ["ready", "not_ready", "mismatch"]
    : ["ready", "not_ready", "mismatch", "unknown"];
}

function stateHint(state: Readiness, activated: boolean): string {
  switch (state) {
    case "ready":
      return activated
        ? "The identity advertises the new fork."
        : "The identity names this fork in its own schedule: the execution fork ID Next field, or the consensus record's next fork version and epoch.";
    case "not_ready":
      return activated
        ? "The identity still advertises the fork before this one."
        : "The identity advertises no next fork: Next is 0, or the next fork epoch is far-future.";
    case "mismatch":
      return "The identity advertises a next fork that is not this one: a custom or overridden fork time, or a client bug in how it encodes the schedule.";
    case "unknown":
      return "The record carries no eth2 entry, or its digest does not match the fork observed over Status, so its schedule cannot be read.";
    case "stale":
      return "The identity is not on the current fork.";
  }
}

function countdown(target: number, now: number): string {
  const s = target - now;
  if (s <= 0) return `activated ${durationAgo(-s)}`;
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `activates in ${d}d ${h}h` : `activates in ${h}h ${m}m`;
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
  activated,
  total,
}: {
  counts: ReadinessCounts;
  activated: boolean;
  total: number;
}) {
  const present = STATES.filter((s) => counts[s] > 0);
  return (
    <span
      className="rd-bar"
      role="img"
      aria-label={present
        .map((s) => `${num(counts[s])} ${stateLabel(s, activated)}`)
        .join(", ")}
    >
      {present.map((s) => (
        <span
          key={s}
          className="rd-seg"
          style={{ flexGrow: counts[s], background: STATE_COLOR[s] }}
          title={`${stateLabel(s, activated)}: ${num(counts[s])} (${pct(counts[s], total)})`}
        />
      ))}
    </span>
  );
}

function Legend({
  layer,
  activated,
}: {
  layer: "el" | "cl";
  activated: boolean;
}) {
  return (
    <div className="rd-legend">
      {legendStates(layer, activated).map((s) => (
        <span key={s} title={stateHint(s, activated)}>
          <span className="swatch" style={{ background: STATE_COLOR[s] }} />
          {stateLabel(s, activated)}
        </span>
      ))}
      <Link to="/about#fork-readiness">What these mean</Link>
    </div>
  );
}

function hasRelease(r?: ClientRelease): boolean {
  return !!r?.min_versions?.length && !r.outdated;
}

function releaseText(r?: ClientRelease): string {
  if (!r) return "not tracked";
  if (r.outdated) return "table outdated for this fork date";
  if (r.min_versions?.length) return `✓ ${r.min_versions.join(" / ")}`;
  if (r.prerelease) return `pre-release ${r.prerelease}`;
  return "no release yet";
}

const VERSION_BADGE: Record<string, string> = {
  meets: "release ≥ minimum",
  below: "older release",
  dev_build: "dev / rc build",
  mixed: "mixed builds",
};

function nodesLink(layer: "el" | "cl", params: Record<string, string>) {
  return {
    pathname: nodesPath(layer),
    search: new URLSearchParams({ ...params, fork: "all" }).toString(),
  };
}

function ReadinessCells({
  counts,
  total,
  activated,
  ready,
}: {
  counts: ReadinessCounts;
  total: number;
  activated: boolean;
  ready?: ReactNode;
}) {
  return (
    <>
      <td className="rd-bar-cell">
        <StackedBar counts={counts} activated={activated} total={total} />
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
  activated,
}: {
  layer: "el" | "cl";
  c: ClientReadiness;
  release?: ClientRelease;
  activated: boolean;
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
        <td className={hasRelease(release) ? "rd-rel ok" : "rd-rel"}>
          {releaseText(release)}
        </td>
        <ReadinessCells
          counts={c.counts}
          total={c.total}
          activated={activated}
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
            <ReadinessCells
              counts={v.counts}
              total={v.total}
              activated={activated}
            />
          </tr>
        ))}
    </>
  );
}

function LayerCard({
  layer,
  data,
  releases,
  activated,
}: {
  layer: "el" | "cl";
  data: LayerReadiness;
  releases: ClientRelease[];
  activated: boolean;
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
      <Legend layer={layer} activated={activated} />
      <div className="table-wrap">
        <table className="nodes-table">
          <thead>
            <tr>
              <th>Client</th>
              <th>Release with the fork</th>
              <th>Readiness</th>
              <th className="num">{activated ? "Upgraded" : "Ready"}</th>
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
                activated={activated}
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
        <div className="card">
          <p className="empty">
            No fork is scheduled on <span className="net-name">{network}</span>.
            The tracker follows the next execution fork in the go-ethereum chain
            configuration and the next consensus fork in the crawler&apos;s
            schedule, and keeps a fork for 14 days after it activates unless a
            later fork is scheduled.
          </p>
        </div>
      </div>
    );
  }

  const activated = data.phase === "activated";
  const activation =
    data.fork.el?.time ??
    (data.fork.cl ? Date.parse(data.fork.cl.time) / 1000 : undefined);
  const layers = (["el", "cl"] as const).filter((l) => data.layers[l]);
  const tiles = layers.flatMap((l) => {
    const c = data.layers[l]!.counts;
    const pool = readyPool(c);
    const share = readyShare(c);
    return [
      {
        label: `${l.toUpperCase()} ${activated ? "upgraded" : "ready"}`,
        value: c.ready,
        hint:
          share === null
            ? undefined
            : `${(share * 100).toFixed(1)}% of ${num(pool)}`,
      },
      {
        label: `${l.toUpperCase()} ${activated ? "left behind" : "not scheduled"}`,
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
  const releases = data.releases;
  const released = releases.filter(hasRelease).length;
  if (releases.length)
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
          <span className={activated ? "phase-badge activated" : "phase-badge"}>
            {data.phase}
          </span>
        </h1>
        <p className="sub">
          {activation && (
            <>
              <strong>{countdown(activation, now)}</strong> · {utc(activation)}
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

      {err && <div className="error">API unreachable: {err}</div>}

      <section className="disclaimer-banner" aria-label="Methodology">
        <span>
          {activated
            ? "Upgraded means the identity advertises the new fork; left behind means it still advertises the fork before it."
            : "Ready means the identity itself advertises the fork: the execution fork ID names the fork time, or the consensus record names the fork version and epoch."}{" "}
          This is the node&apos;s own schedule, not its version string. Release
          labels come from a hand-curated table, updated {data.releases_updated}
          . <Link to="/about#fork-readiness">Methodology</Link>
        </span>
      </section>

      <StatTiles tiles={tiles} />

      <div className="card">
        <h3>Adoption over time</h3>
        <p className="card-subtitle">
          Share of current-fork identities that advertise the fork before
          activation, and that upgraded after it, from the crawler&apos;s
          15-minute history.{" "}
          {layers.map((l) => (
            <span key={l} className="rd-key">
              <span className="swatch" style={{ background: TREND_COLOR[l] }} />
              {layerName(l)}
            </span>
          ))}
        </p>
        <ReadinessTrend
          points={data.history?.points ?? []}
          activation={activation}
          layers={layers}
        />
      </div>

      {layers.map((l) => (
        <LayerCard
          key={l}
          layer={l}
          data={data.layers[l]!}
          releases={releases}
          activated={activated}
        />
      ))}

      {releases.length > 0 && (
        <div className="card">
          <h3>Client releases</h3>
          <p className="card-subtitle">
            First release of each client that ships the {data.fork.name}{" "}
            schedule for <span className="net-name">{network}</span>. Updated{" "}
            {data.releases_updated}.
          </p>
          <div className="table-wrap">
            <table className="nodes-table">
              <thead>
                <tr>
                  <th>Client</th>
                  <th>Layer</th>
                  <th>Release</th>
                  <th>Date</th>
                </tr>
              </thead>
              <tbody>
                {releases.map((r) => (
                  <tr key={r.layer + r.client}>
                    <td>{r.client}</td>
                    <td>{layerName(r.layer)}</td>
                    <td className={hasRelease(r) ? "rd-rel ok" : "rd-rel"}>
                      {r.url ? (
                        <a href={r.url} target="_blank" rel="noreferrer">
                          {releaseText(r)}
                        </a>
                      ) : (
                        releaseText(r)
                      )}
                    </td>
                    <td>{r.released ?? "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
