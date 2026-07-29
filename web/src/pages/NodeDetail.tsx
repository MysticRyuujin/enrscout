import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { ApiError, fetchNode } from "../api";
import { NETWORK_COLOR, relTime } from "../theme";
import type { Node } from "../types";

function Row({
  label,
  value,
  mono,
}: {
  label: string;
  value: React.ReactNode;
  mono?: boolean;
}) {
  const display =
    value === "" || value === null || value === undefined ? "-" : value;
  return (
    <div className="kv">
      <span className="k">{label}</span>
      <span className={mono ? "v mono" : "v"}>{display}</span>
    </div>
  );
}

function discoveryHistory(score: number) {
  if (score >= 10) return "Consistently resolved";
  if (score >= 6) return "Well established";
  if (score >= 3) return "Recurring";
  return "Lightly observed";
}

export default function NodeDetail() {
  const { key } = useParams();
  const [node, setNode] = useState<Node | null>(null);
  const [state, setState] = useState<"loading" | "ok" | "missing" | "error">(
    "loading",
  );
  useEffect(() => {
    let live = true;
    setNode(null);
    setState("loading");
    fetchNode(key!)
      .then((n) => live && (setNode(n), setState("ok")))
      .catch(
        (e) =>
          live &&
          (setNode(null),
          setState(
            e instanceof ApiError && e.status === 404 ? "missing" : "error",
          )),
      );
    return () => {
      live = false;
    };
  }, [key]);

  if (state === "loading") return <div className="page">Loading…</div>;
  if (state === "missing")
    return (
      <div className="page">
        <p>Node not found. It may have aged out of the current snapshot.</p>
        <Link to="/nodes/execution">← Back to nodes</Link>
      </div>
    );
  if (!node) return <div className="page error">Failed to load node.</div>;

  const layerName =
    node.layer === "cl"
      ? "Consensus"
      : node.layer === "el"
        ? "Execution"
        : node.layer;
  const layerPath =
    node.layer === "cl" ? "/nodes/consensus" : "/nodes/execution";
  const dialTransports = [
    (node.tcp || node.tcp6) && "TCP",
    (node.quic || node.quic6) && "QUIC",
  ].filter(Boolean);
  const fpHint: Record<string, string> = {
    pending: "not probed yet",
    failed: "repeated probes failed - retrying with backoff",
    stale: "last successful fingerprint retained - refresh in progress",
    "n/a": "no supported fingerprint transport",
  };
  const clientValue = node.client || (
    <span className="dim">
      unknown - {fpHint[node.fp_status] ?? "not probed"}
    </span>
  );
  const fingerprintValue = node.fingerprint_at ? (
    <>
      {node.fp_status === "stale"
        ? "Stale identification"
        : "Identified (self-reported name, handshake confirmed)"}{" "}
      <span className="dim">
        · {node.fp_direction || "?"} probe {relTime(node.fingerprint_at)}
      </span>
    </>
  ) : (
    (fpHint[node.fp_status] ?? node.fp_status)
  );

  const loc = [node.city, node.subdivision, node.country]
    .filter(Boolean)
    .join(", ");
  const ports =
    [
      node.tcp && `TCP ${node.tcp}`,
      node.tcp6 && `TCP6 ${node.tcp6}`,
      node.quic && `QUIC ${node.quic}`,
      node.quic6 && `QUIC6 ${node.quic6}`,
      node.udp && `UDP ${node.udp}`,
      node.udp6 && `UDP6 ${node.udp6}`,
    ]
      .filter(Boolean)
      .join(" · ") || "-";
  return (
    <div className="page detail">
      <div className="page-head">
        <Link to={layerPath} className="back">
          ← {layerName} nodes
        </Link>
        <h1>
          <span
            className="net-dot"
            style={{ background: NETWORK_COLOR[node.network] || "#8a97ab" }}
          />
          {node.client || "Unknown client"}{" "}
          <span className="dim">{node.client_version}</span>
        </h1>
        <p className="sub mono">{node.id}</p>
      </div>

      <div className="grid-2">
        <div className="card">
          <h3>Network</h3>
          <Row label="Network" value={node.network || "unclassified"} />
          <Row label="Layer" value={layerName} />
          <Row
            label="Membership evidence"
            value={
              node.membership_source === "status"
                ? "Status-verified (authenticated handshake)"
                : node.membership_source === "enr"
                  ? "ENR-claimed (self-declared record)"
                  : "-"
            }
          />
          <Row
            label="Membership verified"
            value={
              node.membership_verified_at
                ? relTime(node.membership_verified_at)
                : "-"
            }
          />
          <Row
            label="Fork readiness"
            value={node.fork_compatible ? "Current" : "Stale / incompatible"}
          />
          <Row
            label="Fork evidence"
            value={
              node.fork_source === "status"
                ? "Authenticated Status"
                : node.fork_source === "enr"
                  ? "Self-signed ENR"
                  : "-"
            }
          />
          <Row
            label="Fork observed"
            value={node.fork_observed_at ? relTime(node.fork_observed_at) : "-"}
          />
          <Row label="Fork hash" value={node.fork_hash} mono />
          <Row label="Fork next" value={node.fork_next || "0"} />
        </div>

        <div className="card">
          <h3>Discovery</h3>
          <Row
            label="Protocols"
            value={
              [node.has_v5 && "discv5", node.has_v4 && "discv4"]
                .filter(Boolean)
                .join(", ") || "-"
            }
          />
          <Row label="IPv4" value={node.ip} mono />
          <Row label="IPv6" value={node.ip6} mono />
          <Row label="Ports" value={ports} />
          <Row
            label="Reachability"
            value={
              node.dialable
                ? `Dialable (${dialTransports.join("/")})`
                : "Discovery-only"
            }
          />
          <Row label="Seq" value={node.seq} />
          <Row
            label="Discovery history"
            value={
              <span title="Successful direct resolutions raise this recurrence score; failed resolutions reduce it. It does not measure fingerprint accuracy.">
                {discoveryHistory(node.score)}{" "}
                <span className="dim">· {node.score}/10</span>
              </span>
            }
          />
          <Row label="First seen" value={relTime(node.first_seen)} />
          <Row label="Last seen" value={relTime(node.last_seen)} />
          <Row
            label="Last resolved"
            value={node.last_resolved ? relTime(node.last_resolved) : "-"}
          />
          <Row label="Pinned" value={node.pinned ? "Yes" : "No"} />
        </div>

        <div className="card">
          <h3>Client</h3>
          <Row label="Client" value={clientValue} />
          <Row label="Version" value={node.client_version} />
          <Row label="OS / arch" value={node.os} />
          <Row label="Language" value={node.lang} />
          <Row label="Capabilities" value={node.capabilities} />
          <Row label="Fingerprint" value={fingerprintValue} />
        </div>

        <div className="card">
          <h3>Location</h3>
          <Row label="Place" value={loc} />
          <Row
            label="Coordinates"
            value={
              node.geolocated
                ? `${node.lat.toFixed(3)}, ${node.lon.toFixed(3)}`
                : "-"
            }
          />
          <Row
            label="Geo accuracy"
            value={
              node.geolocated && node.geo_accuracy_radius_km
                ? `±${node.geo_accuracy_radius_km} km`
                : "-"
            }
          />
          <Row label="ASN" value={node.asn ? `AS${node.asn}` : "-"} />
          <Row label="Organization" value={node.org} />
          <Row
            label="Host type"
            value={
              node.hosting_known
                ? node.hosting
                  ? "Cloud / datacenter"
                  : "Known non-hosting network"
                : "Unknown"
            }
          />
        </div>
      </div>

      <div className="card records-card">
        <h3>Records</h3>
        <Row
          label="enode"
          value={node.enode ? <code className="wrap">{node.enode}</code> : ""}
        />
        <Row
          label="ENR"
          value={node.enr ? <code className="wrap">{node.enr}</code> : ""}
        />
      </div>
    </div>
  );
}
