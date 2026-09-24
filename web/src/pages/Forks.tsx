import { useEffect, useState } from "react";
import { Link } from "react-router";
import { fetchForks } from "../api";
import { useNetwork } from "../network";
import StatTiles from "../components/StatTiles";
import ReadinessTrend, {
  readyShare,
  TREND_COLOR,
} from "../components/ReadinessTrend";
import { durationAgo, layerName, networkColor, num } from "../theme";
import type {
  ClientReadiness,
  ClientRelease,
  ForkReadiness,
  LayerReadiness,
  Readiness,
  ReadinessCounts,
  VersionReadiness,
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
  ready: "#199e70",
  not_ready: "#d95926",
  mismatch: "#9085e9",
  unknown: "#5f6b7e",
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

function Legend({ activated }: { activated: boolean }) {
  return (
    <div className="rd-legend">
      {STATES.map((s) => (
        <span key={s}>
          <span className="swatch" style={{ background: STATE_COLOR[s] }} />
          {stateLabel(s, activated)}
        </span>
      ))}
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
    pathname: layer === "el" ? "/nodes/execution" : "/nodes/consensus",
    search: new URLSearchParams({ ...params, fork: "all" }).toString(),
  };
}

function ClientRow({
  layer,
  c,
  activated,
}: {
  layer: "el" | "cl";
  c: ClientReadiness;
  activated: boolean;
}) {
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
        <td className={hasRelease(c.release) ? "rd-rel ok" : "rd-rel"}>
          {releaseText(c.release)}
        </td>
        <td className="rd-bar-cell">
          <StackedBar counts={c.counts} activated={activated} total={c.total} />
        </td>
        <td className="num">
          {c.client === "Other" ? (
            num(c.counts.ready)
          ) : (
            <Link
              to={nodesLink(layer, {
                client: c.client,
                client_exact: "yes",
                identified: "recent",
                readiness: "ready",
              })}
            >
              {num(c.counts.ready)}
            </Link>
          )}{" "}
          / {num(c.total)}
        </td>
        <td className="num">{pct(c.counts.ready, c.total)}</td>
      </tr>
      {open &&
        c.versions.map((v: VersionReadiness) => (
          <tr key={v.version} className="rd-version">
            <td className="mono">{v.version}</td>
            <td>{v.release ? (VERSION_BADGE[v.release] ?? "") : ""}</td>
            <td className="rd-bar-cell">
              <StackedBar
                counts={v.counts}
                activated={activated}
                total={v.total}
              />
            </td>
            <td className="num">
              {num(v.counts.ready)} / {num(v.total)}
            </td>
            <td className="num">{pct(v.counts.ready, v.total)}</td>
          </tr>
        ))}
    </>
  );
}

function LayerCard({
  layer,
  data,
  activated,
}: {
  layer: "el" | "cl";
  data: LayerReadiness;
  activated: boolean;
}) {
  const unidentified = STATES.reduce((a, s) => a + data.unidentified[s], 0);
  return (
    <div className="card">
      <h3>{layerName(layer)} clients</h3>
      <p className="card-subtitle">
        {num(data.total)} identities. Client rows count identities with a
        verified handshake in the last 7 days; everything else is in the last
        row. Ready counts link to the matching nodes.
      </p>
      <Legend activated={activated} />
      <div className="table-wrap">
        <table className="rd-table">
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
            {(data.clients ?? []).map((c) => (
              <ClientRow
                key={c.client}
                layer={layer}
                c={c}
                activated={activated}
              />
            ))}
            {unidentified > 0 && (
              <tr className="rd-unidentified">
                <td>Not recently identified</td>
                <td />
                <td className="rd-bar-cell">
                  <StackedBar
                    counts={data.unidentified}
                    activated={activated}
                    total={unidentified}
                  />
                </td>
                <td className="num">
                  {num(data.unidentified.ready)} / {num(unidentified)}
                </td>
                <td className="num">
                  {pct(data.unidentified.ready, unidentified)}
                </td>
              </tr>
            )}
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
    const timer = window.setInterval(() => {
      setNow(Date.now() / 1000);
      if (document.visibilityState === "visible") void load();
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
            No fork is scheduled on {network}. The tracker follows the next
            execution fork in the go-ethereum chain configuration and the next
            consensus fork in the crawler&apos;s schedule, and keeps a fork for
            14 days after it activates unless a later fork is scheduled.
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
    const pool = c.ready + c.not_ready + c.mismatch + c.unknown;
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
  const releases = data.releases ?? [];
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
          activated={activated}
        />
      ))}

      {releases.length > 0 && (
        <div className="card">
          <h3>Client releases</h3>
          <p className="card-subtitle">
            First release of each client that ships the {data.fork.name}{" "}
            schedule for {network}. Updated {data.releases_updated}.
          </p>
          <div className="table-wrap">
            <table className="rd-table">
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
