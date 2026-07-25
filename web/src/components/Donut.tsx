import { CATEGORICAL, num, OTHER_COLOR } from "../theme";

export interface DonutProps {
  title: string;
  subtitle?: string;
  data: [string, number][];
  limit?: number;
  color?: (name: string) => string;
}

export default function Donut({
  title,
  subtitle,
  data,
  limit = 6,
  color,
}: DonutProps) {
  const total = data.reduce((a, [, c]) => a + c, 0);
  if (total === 0) {
    return (
      <div className="card donut-card">
        <h3>{title}</h3>
        {subtitle && <p className="card-subtitle">{subtitle}</p>}
        <p className="empty">No data yet.</p>
      </div>
    );
  }

  // Fold any server-supplied "Other" into the overflow bucket, not a second slice.
  const named = data.filter(([name]) => name !== "Other");
  const otherFromData = data.reduce(
    (a, [name, c]) => (name === "Other" ? a + c : a),
    0,
  );
  const top = named.slice(0, limit);
  const rest =
    named.slice(limit).reduce((a, [, c]) => a + c, 0) + otherFromData;
  const slices: { name: string; count: number; color: string }[] = top.map(
    ([name, count], i) => ({
      name,
      count,
      color: color ? color(name) : CATEGORICAL[i % CATEGORICAL.length],
    }),
  );
  if (rest > 0) slices.push({ name: "Other", count: rest, color: OTHER_COLOR });

  const R = 52;
  const C = 2 * Math.PI * R;
  let acc = 0;

  return (
    <div className="card donut-card">
      <h3>{title}</h3>
      {subtitle && <p className="card-subtitle">{subtitle}</p>}
      <div className="donut-body">
        <svg viewBox="0 0 130 130" className="donut">
          <circle
            cx="65"
            cy="65"
            r={R}
            fill="none"
            stroke="#1a2233"
            strokeWidth="16"
          />
          {slices.map((s) => {
            const len = (s.count / total) * C;
            // Overdraw a hair so the next arc hides the sub-pixel seam that would otherwise expose the track between butt-capped strokes.
            const drawn = Math.min(len + 0.75, C);
            const el = (
              <circle
                key={s.name}
                cx="65"
                cy="65"
                r={R}
                fill="none"
                stroke={s.color}
                strokeWidth="16"
                strokeDasharray={`${drawn} ${C - drawn}`}
                strokeDashoffset={-acc}
                transform="rotate(-90 65 65)"
              />
            );
            acc += len;
            return el;
          })}
          <text x="65" y="61" textAnchor="middle" className="donut-total">
            {num(total)}
          </text>
          <text x="65" y="77" textAnchor="middle" className="donut-sub">
            identities
          </text>
        </svg>
        <ul className="donut-legend">
          {slices.map((s) => (
            <li key={s.name}>
              <span className="swatch" style={{ background: s.color }} />
              <span className="lg-name">{s.name}</span>
              <span
                className="lg-val"
                title={`${num(s.count)} identities`}
              >{`${((s.count / total) * 100).toFixed(0)}%`}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
