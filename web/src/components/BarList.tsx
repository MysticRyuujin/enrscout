import { ACCENT, num } from "../theme";

export interface BarListProps {
  title: string;
  rows: [string, number][];
  total: number;
  color?: (name: string) => string;
}

export default function BarList({ title, rows, total, color }: BarListProps) {
  if (rows.length === 0) {
    return (
      <div className="card barlist">
        <h3>{title}</h3>
        <p className="empty">No data yet.</p>
      </div>
    );
  }
  const max = Math.max(...rows.map((r) => r[1]), 1);
  return (
    <div className="card barlist">
      <h3>{title}</h3>
      {rows.map(([name, count]) => {
        return (
          <div className="bar-row" key={name}>
            <span className="bar-name">{name}</span>
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
              title={`${(total > 0 ? (count / total) * 100 : 0).toFixed(1)}%`}
            >
              {num(count)}
            </span>
          </div>
        );
      })}
    </div>
  );
}
