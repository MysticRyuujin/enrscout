import { useEffect, useState } from "react";
import { fetchStats } from "../api";
import { num, topN } from "../theme";

export default function ClientVersions({
  network,
  clients,
  color,
}: {
  network: string;
  clients: string[];
  color: string;
}) {
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<Record<string, number>>({});

  useEffect(() => {
    if (!clients.length) setSelected("");
    else if (!selected || !clients.includes(selected)) setSelected(clients[0]);
  }, [clients, selected]);

  useEffect(() => {
    if (!selected) return;
    let live = true;
    const load = () =>
      fetchStats(network, selected)
        .then((s) => live && setVersions(s.by_version || {}))
        .catch(() => live && setVersions({}));
    setVersions({});
    load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, 60000);
    return () => {
      live = false;
      window.clearInterval(timer);
    };
  }, [network, selected]);

  const rows = topN(versions, 12);
  const max = Math.max(...rows.map((r) => r[1]), 1);

  return (
    <div className="card">
      <div className="cv-head">
        <h3>Client versions</h3>
        <select value={selected} onChange={(e) => setSelected(e.target.value)}>
          {clients.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
      </div>
      {rows.length === 0 ? (
        <p className="empty">
          No version data for {selected || "this client"} yet.
        </p>
      ) : (
        rows.map(([name, count]) => (
          <div className="bar-row" key={name}>
            <span className="bar-name mono">{name}</span>
            <span className="bar-track">
              <span
                className="bar-fill"
                style={{ width: `${(count / max) * 100}%`, background: color }}
              />
            </span>
            <span className="bar-count">{num(count)}</span>
          </div>
        ))
      )}
    </div>
  );
}
