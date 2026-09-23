import { ACCENT, num } from "../theme";

export interface BarListProps {
  title: string;
  rows: [string, number][];
  total: number;
  color?: (name: string) => string;
}

// BarRows draws bars scaled to the largest row. total, when given, adds a share tooltip.
export function BarRows({
  rows,
  total,
  color,
  mono,
}: {
  rows: [string, number][];
  total?: number;
  color?: (name: string) => string;
  mono?: boolean;
}) {
  const max = Math.max(...rows.map((r) => r[1]), 1);
  return rows.map(([name, count]) => (
    <div className="bar-row" key={name}>
      <span className={mono ? "bar-name mono" : "bar-name"}>{name}</span>
      <span className="bar-track">
        <span
          className="bar-fill"
          style={{
            width: `${(count / max) * 100}%`,
            background: color ? color(name) : ACCENT,
          }}
        />
      </span>
      <span
        className="bar-count"
        title={
          total === undefined
            ? undefined
            : `${(total > 0 ? (count / total) * 100 : 0).toFixed(1)}%`
        }
      >
        {num(count)}
      </span>
    </div>
  ));
}

export default function BarList({ title, rows, total, color }: BarListProps) {
  return (
    <div className="card barlist">
      <h3>{title}</h3>
      {rows.length === 0 ? (
        <p className="empty">No data yet.</p>
      ) : (
        <BarRows rows={rows} total={total} color={color} />
      )}
    </div>
  );
}
