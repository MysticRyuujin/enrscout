import { useEffect, useRef, useState } from "react";
import { ACCENT, CATEGORICAL, num } from "../theme";
import type { ReadinessCounts, ReadinessPoint } from "../types";

export const TREND_COLOR = { el: ACCENT, cl: CATEGORICAL[2] } as const;

const H = 220;
const PAD = { top: 12, right: 100, bottom: 28, left: 40 };

// Stale rows are off the current fork before activation and long gone after it, so they are not
// part of the population a fork can be ready in.
export function readyPool(c: Partial<ReadinessCounts>): number {
  return (
    (c.ready ?? 0) +
    (c.pending ?? 0) +
    (c.not_ready ?? 0) +
    (c.mismatch ?? 0) +
    (c.unknown ?? 0)
  );
}

export function readyShare(
  c: Partial<ReadinessCounts> | undefined,
): number | null {
  if (!c) return null;
  const pool = readyPool(c);
  return pool ? (c.ready ?? 0) / pool : null;
}

// Pending rows scheduled the fork before activation and have not been seen since, so counting them
// keeps the adoption line continuous across activation instead of dropping to the re-observed few.
function scheduledShare(
  c: Partial<ReadinessCounts> | undefined,
): number | null {
  if (!c) return null;
  const pool = readyPool(c);
  return pool ? ((c.ready ?? 0) + (c.pending ?? 0)) / pool : null;
}

function linePath(
  values: (number | null)[],
  points: ReadinessPoint[],
  x: (t: number) => number,
  y: (v: number) => number,
): string {
  let d = "";
  for (const [i, v] of values.entries()) {
    if (v === null) continue;
    d += `${d && values[i - 1] !== null ? "L" : "M"}${x(points[i].at).toFixed(1)},${y(v).toFixed(1)}`;
  }
  return d;
}

function lastValue(values: (number | null)[], points: ReadinessPoint[]) {
  for (let i = values.length - 1; i >= 0; i--) {
    const v = values[i];
    if (v !== null) return { at: points[i].at, value: v };
  }
  return null;
}

function fmtClock(unix: number): string {
  return new Date(unix * 1000).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

function fmtTime(unix: number): string {
  return new Date(unix * 1000).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export default function ReadinessTrend({
  points,
  activation,
  layers,
}: {
  points: ReadinessPoint[];
  activation?: number;
  layers: ("el" | "cl")[];
}) {
  const wrap = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(720);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) =>
      setWidth(Math.max(280, Math.floor(entry.contentRect.width))),
    );
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  if (points.length === 0) {
    return (
      <div className="trend" ref={wrap}>
        <p className="empty">
          No history yet. The crawler records a point every 15 minutes, and
          every minute near activation, once it tracks this fork.
        </p>
      </div>
    );
  }

  const t0 = points[0].at;
  const tLast = points[points.length - 1].at;
  // Show the time left to activation only once the history is at least as long, or a short
  // history is squeezed against the left edge.
  const t1 =
    activation && activation > tLast && activation - tLast <= tLast - t0
      ? activation
      : Math.max(tLast, t0 + 1);
  const plotW = width - PAD.left - PAD.right;
  const plotH = H - PAD.top - PAD.bottom;
  const x = (t: number) => PAD.left + ((t - t0) / (t1 - t0)) * plotW;
  const y = (share: number) => PAD.top + (1 - share) * plotH;

  const series = layers.map((layer) => {
    const counts = points.map((p) => (layer === "el" ? p.el : p.cl));
    const values = counts.map(scheduledShare);
    const upgraded = counts.map((c, i) =>
      activation !== undefined && points[i].at >= activation
        ? readyShare(c)
        : null,
    );
    return {
      layer,
      values,
      upgraded,
      d: linePath(values, points, x, y),
      dUpgraded: linePath(upgraded, points, x, y),
      last: lastValue(values, points),
      lastUpgraded: lastValue(upgraded, points),
    };
  });

  const labelY: Record<string, number> = {};
  const placed = series
    .flatMap((s) => [
      ...(s.last ? [{ key: s.layer, y: y(s.last.value) + 4 }] : []),
      ...(s.lastUpgraded
        ? [{ key: `${s.layer}-up`, y: y(s.lastUpgraded.value) + 4 }]
        : []),
    ])
    .sort((a, b) => a.y - b.y);
  placed.forEach((l, i) => {
    const floor = i === 0 ? PAD.top + 10 : placed[i - 1].y + 13;
    l.y = Math.max(l.y, floor);
  });
  for (let i = placed.length - 1; i >= 0; i--) {
    const ceiling =
      i === placed.length - 1 ? PAD.top + plotH + 4 : placed[i + 1].y - 13;
    placed[i].y = Math.min(placed[i].y, ceiling);
    labelY[placed[i].key] = placed[i].y;
  }

  const onMove = (e: React.PointerEvent<SVGRectElement>) => {
    const box = e.currentTarget.getBoundingClientRect();
    const t = t0 + ((e.clientX - box.left) / box.width) * (t1 - t0);
    let best = 0;
    for (let i = 1; i < points.length; i++) {
      if (Math.abs(points[i].at - t) < Math.abs(points[best].at - t)) best = i;
    }
    setHover(best);
  };

  const hp = hover === null ? null : points[hover];
  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="trend" ref={wrap}>
      <svg
        width={width}
        height={H}
        role="img"
        aria-label="Share of current-fork identities ready for the fork over time"
      >
        {ticks.map((v) => (
          <g key={v}>
            <line
              className="trend-grid"
              x1={PAD.left}
              x2={PAD.left + plotW}
              y1={y(v)}
              y2={y(v)}
            />
            <text
              className="trend-axis"
              x={PAD.left - 6}
              y={y(v) + 4}
              textAnchor="end"
            >
              {v * 100}%
            </text>
          </g>
        ))}
        <text className="trend-axis" x={PAD.left} y={H - 8}>
          {fmtTime(t0)}
        </text>
        <text
          className="trend-axis"
          x={PAD.left + plotW}
          y={H - 8}
          textAnchor="end"
        >
          {new Date(t0 * 1000).toDateString() ===
          new Date(t1 * 1000).toDateString()
            ? fmtClock(t1)
            : fmtTime(t1)}
        </text>
        {activation && activation >= t0 && activation <= t1 && (
          <g>
            <line
              className="trend-activation"
              x1={x(activation)}
              x2={x(activation)}
              y1={PAD.top}
              y2={PAD.top + plotH}
            />
            <text
              className="trend-axis"
              x={x(activation) - 4}
              y={PAD.top + 10}
              textAnchor="end"
            >
              activation
            </text>
          </g>
        )}
        {series.map((s) => (
          <g key={s.layer}>
            <path
              d={s.d}
              fill="none"
              stroke={TREND_COLOR[s.layer]}
              strokeWidth={2}
              strokeLinejoin="round"
            />
            {s.dUpgraded && (
              <path
                d={s.dUpgraded}
                fill="none"
                stroke={TREND_COLOR[s.layer]}
                strokeWidth={2}
                strokeDasharray="4 3"
                strokeLinejoin="round"
              />
            )}
            {s.lastUpgraded !== null && (
              <text
                className="trend-label"
                x={x(s.lastUpgraded.at) + 6}
                y={labelY[`${s.layer}-up`]}
              >
                {s.layer.toUpperCase()}{" "}
                {(s.lastUpgraded.value * 100).toFixed(1)}% seen
              </text>
            )}
            {s.last !== null && (
              <circle
                cx={x(s.last.at)}
                cy={y(s.last.value)}
                r={3.5}
                fill={TREND_COLOR[s.layer]}
                className="trend-dot"
              />
            )}
            {s.last !== null && (
              <text
                className="trend-label"
                x={x(s.last.at) + 6}
                y={labelY[s.layer]}
              >
                {s.layer.toUpperCase()} {(s.last.value * 100).toFixed(1)}%
              </text>
            )}
          </g>
        ))}
        {hp && (
          <g>
            <line
              className="trend-cross"
              x1={x(hp.at)}
              x2={x(hp.at)}
              y1={PAD.top}
              y2={PAD.top + plotH}
            />
            {series.map((s) => {
              const v = s.values[hover!];
              return v === null ? null : (
                <circle
                  key={s.layer}
                  cx={x(hp.at)}
                  cy={y(v)}
                  r={4}
                  fill={TREND_COLOR[s.layer]}
                  className="trend-dot"
                />
              );
            })}
          </g>
        )}
        <rect
          x={PAD.left}
          y={PAD.top}
          width={plotW}
          height={plotH}
          fill="transparent"
          onPointerMove={onMove}
          onPointerLeave={() => setHover(null)}
        />
      </svg>
      {hp && (
        <div
          className="trend-tip"
          style={{ left: Math.min(x(hp.at) + 12, width - 180), top: PAD.top }}
        >
          <div className="trend-tip-time">{fmtTime(hp.at)}</div>
          {series.map((s) => {
            const c = s.layer === "el" ? hp.el : hp.cl;
            const v = s.values[hover!];
            if (v === null || !c) return null;
            const up = s.upgraded[hover!];
            return (
              <div key={s.layer}>
                <span
                  className="swatch"
                  style={{ background: TREND_COLOR[s.layer] }}
                />
                {s.layer === "el" ? "Execution" : "Consensus"}{" "}
                {(v * 100).toFixed(1)}%
                {up === null
                  ? ` (${num(c.ready ?? 0)} scheduled)`
                  : ` (${num(c.ready ?? 0)} upgraded, ${num(c.pending ?? 0)} pending)`}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
