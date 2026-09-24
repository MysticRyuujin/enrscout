import { useEffect, useState } from "react";
import { Link } from "react-router";
import { fetchStats } from "../api";
import { nodesPath, num, topN, whileVisible } from "../theme";
import { BarRows } from "./BarList";

const SHOWN_VERSIONS = 12;

export interface LayerClient {
  client: string;
  layer: "el" | "cl";
}

const choiceKey = (c: LayerClient) => `${c.layer}:${c.client}`;

export default function ClientVersions({
  network,
  clients,
  color,
}: {
  network: string;
  clients: LayerClient[];
  color: string;
}) {
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<Record<string, number>>({});
  const choice = clients.find((c) => choiceKey(c) === selected);

  useEffect(() => {
    if (!clients.length) setSelected("");
    else if (!clients.some((c) => choiceKey(c) === selected))
      setSelected(choiceKey(clients[0]));
  }, [clients, selected]);

  useEffect(() => {
    if (!choice) return;
    let live = true;
    const load = () =>
      fetchStats(network, choice.client, choice.layer)
        .then((s) => live && setVersions(s.by_version || {}))
        .catch(() => live && setVersions({}));
    setVersions({});
    load();
    const timer = window.setInterval(whileVisible(load), 60000);
    return () => {
      live = false;
      window.clearInterval(timer);
    };
  }, [network, choice?.client, choice?.layer]);

  const all = topN(versions, Infinity);
  const total = all.reduce((sum, [, n]) => sum + n, 0);
  const rows = all.slice(0, SHOWN_VERSIONS);
  const rest = all.slice(SHOWN_VERSIONS);
  if (rest.length) {
    rows.push([
      `Other (${rest.length} versions)`,
      rest.reduce((sum, [, n]) => sum + n, 0),
    ]);
  }
  const bothLayers = new Set(
    clients
      .filter((c) =>
        clients.some((o) => o.client === c.client && o.layer !== c.layer),
      )
      .map((c) => c.client),
  );
  const nodesLink = choice && {
    pathname: nodesPath(choice.layer),
    search: new URLSearchParams({
      network,
      client: choice.client,
      client_exact: "yes",
      identified: "recent",
    }).toString(),
  };

  return (
    <div className="card">
      <div className="cv-head">
        <h3>Client versions</h3>
        <select value={selected} onChange={(e) => setSelected(e.target.value)}>
          {clients.map((c) => (
            <option key={choiceKey(c)} value={choiceKey(c)}>
              {bothLayers.has(c.client)
                ? `${c.client} (${c.layer.toUpperCase()})`
                : c.client}
            </option>
          ))}
        </select>
      </div>
      {rows.length === 0 ? (
        <p className="empty">
          No version data for {choice?.client || "this client"} yet.
        </p>
      ) : (
        <>
          <p className="card-subtitle">
            {num(total)} current-fork identities with a verified handshake in
            the last 7 days
            {nodesLink && (
              <>
                {" · "}
                <Link to={nodesLink}>view nodes</Link>
              </>
            )}
          </p>
          <BarRows rows={rows} total={total} color={() => color} mono />
        </>
      )}
    </div>
  );
}
