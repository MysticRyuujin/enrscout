import { useEffect, useMemo, useState } from "react";
import { fetchMap, fetchMeta, fetchStats } from "../api";
import { useNetwork } from "../network";
import StatTiles, { type TileFilter } from "../components/StatTiles";
import BarList from "../components/BarList";
import Donut from "../components/Donut";
import ClientVersions from "../components/ClientVersions";
import WorldMap from "../components/WorldMap";
import {
  clientColor,
  durationAgo,
  hexRGB,
  NETWORK_COLOR,
  num,
  OTHER_COLOR,
  topN,
} from "../theme";
import {
  pointCGC,
  pointClient,
  pointHosting,
  pointIPv6,
  pointLayer,
  pointVerified,
} from "../types";
import type { CompactMap, MapPoint, Meta, Stats } from "../types";

const STALE_SECONDS = 900;
const SUMMARY_REFRESH_MS = 60_000;
const MAP_REFRESH_MS = 5 * 60_000;
const MAP_PREDICATES: Record<string, (p: MapPoint) => boolean> = {
  el: (p) => pointLayer(p) === "el",
  cl: (p) => pointLayer(p) === "cl",
  ipv6: pointIPv6,
  cloud: pointHosting,
  verified: pointVerified,
  supernode: (p) => pointCGC(p) >= 128,
};

function identifiedCoverage(
  byClient: Record<string, number>,
  byDirection: Record<string, number>,
  staleIdentified: number,
  layerTotal: number,
): string {
  const identified = Object.values(byClient).reduce(
    (sum, count) => sum + count,
    0,
  );
  const percent = layerTotal
    ? ((identified / layerTotal) * 100).toFixed(1)
    : "0.0";
  const inbound = byDirection["inbound"] ?? 0;
  const inboundPct = identified ? Math.round((inbound / identified) * 100) : 0;
  const outboundPct = identified ? 100 - inboundPct : 0;
  let summary = `${num(identified)} of ${num(layerTotal)} current-fork identities (${percent}%). Direction: ${inboundPct}% inbound / ${outboundPct}% outbound.`;
  if (staleIdentified > 0)
    summary += ` ${num(staleIdentified)} stale identifications excluded.`;
  return summary;
}

// Tile filters run over the points the response actually returned, so a truncated map cannot report
// a filtered count against the full located population - the omitted points were never evaluated.
function mapCoverageCaption(
  total: number,
  rendered: number,
  truncated: boolean,
  filtered: number | null,
): string {
  if (filtered !== null) {
    return truncated
      ? `${num(filtered)} of ${num(rendered)} rendered · ${num(total)} identities located`
      : `${num(filtered)} of ${num(total)} identities located`;
  }
  return truncated
    ? `${num(total)} identities located · map renders ${num(rendered)}`
    : `${num(total)} identities located`;
}

const WARMUP_HOURS = 48;

function warmupProgress(
  endsAt: string | undefined,
): { hour: number; pct: number } | null {
  if (!endsAt) return null;
  const ends = new Date(endsAt).getTime();
  if (Number.isNaN(ends)) return null;
  const start = ends - WARMUP_HOURS * 3600000;
  const elapsed = (Date.now() - start) / 3600000;
  const clamped = Math.max(0, Math.min(WARMUP_HOURS, elapsed));
  return {
    hour: Math.min(WARMUP_HOURS, Math.floor(clamped) + 1),
    pct: (clamped / WARMUP_HOURS) * 100,
  };
}

export default function Overview() {
  const { network } = useNetwork();
  const [stats, setStats] = useState<Stats | null>(null);
  const [map, setMap] = useState<CompactMap | null>(null);
  const [meta, setMeta] = useState<Meta | null>(null);
  const [errSummary, setErrSummary] = useState<string | null>(null);
  const [errMap, setErrMap] = useState<string | null>(null);
  const [mapFilters, setMapFilters] = useState<Record<string, TileFilter>>({});
  const color = NETWORK_COLOR[network] || "#8a97ab";

  const cycleFilter = (key: string) =>
    setMapFilters((f) => {
      const cur = f[key] ?? null;
      const next: TileFilter =
        cur === null ? "only" : cur === "only" ? "hide" : null;
      const out = { ...f, [key]: next };
      if (next && key === "el") {
        out.cl = null;
        out.supernode = null;
      }
      if (next && key === "cl") out.el = null;
      return out;
    });

  // Easter egg: the consensus tile has a fourth stop, supernodes (cgc >= 128).
  const cycleConsensusFilter = () =>
    setMapFilters((f) => {
      const out: Record<string, TileFilter> = {
        ...f,
        el: null,
        supernode: null,
      };
      if (f.supernode) {
        out.cl = "hide";
      } else if (f.cl === "only") {
        out.cl = "only";
        out.supernode = "only";
      } else if (f.cl === "hide") {
        out.cl = null;
        out.el = f.el ?? null;
      } else {
        out.cl = "only";
      }
      return out;
    });

  useEffect(() => {
    let live = true;
    setStats(null);
    setMap(null);
    setMeta(null);
    setErrSummary(null);
    setErrMap(null);
    setMapFilters({});
    const loadSummary = async () => {
      try {
        const [s, currentMeta] = await Promise.all([
          fetchStats(network),
          fetchMeta(),
        ]);
        if (live) {
          setStats(s);
          setMeta(currentMeta);
          setErrSummary(null);
        }
      } catch (e) {
        if (live) setErrSummary(e instanceof Error ? e.message : String(e));
      }
    };
    const loadMap = async () => {
      try {
        const currentMap = await fetchMap(network);
        if (live) {
          setMap(currentMap);
          setErrMap(null);
        }
      } catch (e) {
        if (live) setErrMap(e instanceof Error ? e.message : String(e));
      }
    };
    const whileVisible = (load: () => Promise<void>) => () => {
      if (document.visibilityState === "visible") void load();
    };

    void loadSummary();
    void loadMap();
    const summaryTimer = setInterval(
      whileVisible(loadSummary),
      SUMMARY_REFRESH_MS,
    );
    const mapTimer = setInterval(whileVisible(loadMap), MAP_REFRESH_MS);
    return () => {
      live = false;
      clearInterval(summaryTimer);
      clearInterval(mapTimer);
    };
  }, [network]);

  const stale = meta?.age_seconds != null && meta.age_seconds > STALE_SECONDS;

  const mapPoints = useMemo(() => {
    const pts = map?.points ?? null;
    const active = Object.entries(mapFilters).filter(([, v]) => v) as [
      string,
      "only" | "hide",
    ][];
    // A cached pre-upgrade payload without the filter fields must not silently empty the map.
    if (!pts || active.length === 0 || (pts[0]?.length ?? 0) < 11) return pts;
    return pts.filter((p) =>
      active.every(([k, v]) => MAP_PREDICATES[k](p) === (v === "only")),
    );
  }, [map, mapFilters]);
  const filtersActive = Object.values(mapFilters).some(Boolean);
  const locatedTotal = map?.total ?? map?.points.length ?? 0;
  const locatedCaption =
    map === null
      ? errMap
        ? "map unavailable"
        : "locating identities…"
      : mapCoverageCaption(
          locatedTotal,
          map.points.length,
          map.truncated ?? false,
          filtersActive ? (mapPoints?.length ?? 0) : null,
        );

  const clientColors = useMemo(() => {
    if (!stats) return null;
    const counts = new Map<string, number>();
    for (const source of [stats.by_client_el, stats.by_client_cl]) {
      for (const [name, count] of Object.entries(source)) {
        counts.set(name, (counts.get(name) ?? 0) + count);
      }
    }
    const m = new Map<string, string>();
    [...counts.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .forEach(([name]) => m.set(name, clientColor(name)));
    return m;
  }, [stats]);

  const colorFor = useMemo(() => {
    return (p: MapPoint): [number, number, number, number] => {
      return [...hexRGB(clientColor(pointClient(p))), 220];
    };
  }, []);
  const colorKey = clientColors
    ? "static-client-palette"
    : "network:" + network;
  const legendLayer =
    mapFilters.el === "only" || mapFilters.cl === "hide"
      ? stats?.by_client_el
      : mapFilters.cl === "only" || mapFilters.el === "hide"
        ? stats?.by_client_cl
        : null;
  const legend = clientColors
    ? [...clientColors.entries()]
        .filter(
          ([name]) =>
            name !== "Other" &&
            (!legendLayer ||
              Object.prototype.hasOwnProperty.call(legendLayer, name)),
        )
        .map(([name, c]) => ({ name, color: c }))
        .concat({ name: "Other", color: OTHER_COLOR })
    : undefined;

  const ipv6pct =
    stats && stats.total
      ? ((stats.ipv6 / stats.total) * 100).toFixed(1) + "%"
      : undefined;
  const hostpct =
    stats && stats.total
      ? ((stats.hosting / stats.total) * 100).toFixed(0) + "%"
      : undefined;
  const clients = stats
    ? topN(stats.by_client, 20)
        .map(([n]) => n)
        .filter((n) => n !== "Other")
    : [];
  const executionCaption = stats
    ? identifiedCoverage(
        stats.by_client_el,
        stats.by_direction_el,
        stats.el_identified_stale,
        stats.execution,
      )
    : null;
  const consensusCaption = stats
    ? identifiedCoverage(
        stats.by_client_cl,
        stats.by_direction_cl,
        stats.cl_identified_stale,
        stats.consensus,
      )
    : null;

  return (
    <div className="page overview">
      <div className="page-head">
        <h1>
          <span className="net-dot" style={{ background: color }} /> {network}{" "}
          network
        </h1>
        <p className="sub">
          {meta?.generated_at ? (
            <>
              Updated{" "}
              <span className={stale ? "freshness stale" : "freshness"}>
                {durationAgo(meta.age_seconds ?? 0)}
              </span>
            </>
          ) : (
            "No snapshot yet"
          )}{" "}
          ·{" "}
          {locatedCaption} · a typical full node contributes one EL and one CL
          identity
        </p>
      </div>

      {(errSummary || errMap) && (
        <div className="error">API unreachable: {errSummary || errMap}</div>
      )}

      {stats?.warming_up &&
        (() => {
          const wp = warmupProgress(stats.warmup_ends_at);
          return (
            <section
              className="warmup-banner"
              role="status"
              aria-label="Dataset warm-up status"
            >
              <span className="warmup-badge">
                <span className="warmup-dot" />
                Warming up
              </span>
              {wp && (
                <span
                  className="warmup-meter"
                  title={`Hour ${wp.hour} of ${WARMUP_HOURS}`}
                >
                  <span
                    className="warmup-fill"
                    style={{ width: `${wp.pct}%` }}
                  />
                </span>
              )}
              <span className="warmup-note">
                {wp ? `Hour ${wp.hour} of ${WARMUP_HOURS}. ` : ""}Crawler hasn't
                been running a full {WARMUP_HOURS} hours yet, so client shares
                may still shift.
              </span>
            </section>
          );
        })()}

      <section className="disclaimer-banner" aria-label="Methodology">
        <span>
          Charts below are an observed, non-random identity sample from a single
          vantage point. Client names and versions are self-reported and
          confirmed only by a completed handshake; shares are of observed
          identities, not machine, operator, validator, or stake share.
        </span>
      </section>

      {stats && (
        <StatTiles
          tiles={[
            { label: "total identities", value: stats.total },
            {
              label: "status-verified",
              value: stats.membership_verified,
              hint: `+ ${num(stats.membership_claimed)} ENR-claimed`,
              filter: mapFilters.verified ?? null,
              onFilter: () => cycleFilter("verified"),
              stateLabels: {
                only: "map: verified only",
                hide: "map: ENR-claimed only",
              },
              title:
                "Status-verified: membership proven in a live authenticated handshake, tied to the node key. " +
                "ENR-claimed: membership only self-declared in the node's signed discovery record. " +
                "Click to filter the map: only → hidden → off. More in About → Membership.",
            },
            {
              label: "execution identities",
              value: stats.execution,
              filter: mapFilters.el ?? null,
              onFilter: () => cycleFilter("el"),
            },
            {
              label: "consensus identities",
              value: stats.consensus,
              filter: mapFilters.cl ?? null,
              onFilter: cycleConsensusFilter,
              stateLabelOverride: mapFilters.supernode
                ? "✨ supernodes"
                : undefined,
            },
            { label: "discv5", value: stats.discv5 },
            { label: "discv4", value: stats.discv4 },
            {
              label: "advertises IPv6",
              value: stats.ipv6,
              hint: ipv6pct,
              filter: mapFilters.ipv6 ?? null,
              onFilter: () => cycleFilter("ipv6"),
            },
            {
              label: "cloud-hosted",
              value: stats.hosting,
              hint: hostpct,
              filter: mapFilters.cloud ?? null,
              onFilter: () => cycleFilter("cloud"),
            },
          ]}
        />
      )}

      {filtersActive && (
        <div className="map-toolbar">
          <button className="map-clear" onClick={() => setMapFilters({})}>
            clear map filters
          </button>
        </div>
      )}

      <WorldMap
        points={mapPoints}
        network={network}
        colorFor={colorFor}
        colorKey={colorKey}
        legend={legend}
      />

      {stats && (
        <div className="grid-2">
          <Donut
            title="Execution client fingerprints"
            subtitle={executionCaption ?? undefined}
            data={topN(stats.by_client_el, 20)}
            color={clientColor}
          />
          <Donut
            title="Consensus client fingerprints"
            subtitle={consensusCaption ?? undefined}
            data={topN(stats.by_client_cl, 20)}
            color={clientColor}
          />
        </div>
      )}

      {stats && (
        <div className="grid-2">
          <Donut
            title="Operating systems (identified sample)"
            subtitle={`${num(Object.values(stats.by_os).reduce((a, b) => a + b, 0))} identities in the current seven-day sample report an operating system.`}
            data={topN(stats.by_os, 20)}
          />
          <ClientVersions network={network} clients={clients} color={color} />
        </div>
      )}

      {stats && (
        <div className="grid-2">
          {/* Deliberately not drill-downs: these counts span both layers, but /nodes only has
              per-layer views, so one would silently show the execution subset of the number clicked. */}
          <BarList
            title="Countries"
            rows={topN(stats.by_country, 12)}
            total={stats.total}
            color={() => color}
          />
          <BarList
            title="Hosting / ASN"
            rows={topN(stats.by_org, 12)}
            total={stats.total}
            color={() => color}
          />
        </div>
      )}
    </div>
  );
}
