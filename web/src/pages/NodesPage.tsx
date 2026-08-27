import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { fetchNodes } from "../api";
import { useNetwork } from "../network";
import { num, relTime, shortId } from "../theme";
import type { NodesResult } from "../types";

const PAGE = 50;
const FILTER_DEBOUNCE_MS = 250;

type PatchFilter = (key: string, value: string) => void;
type NodeSort = "last_seen" | "client" | "cgc";

// Accepted custody expressions: "128" (exact), "4-8" (range), "8+" / ">=8"
// (at least), "<=8" (at most), and strict ">8" / "<8", exact in the integer
// domain (>8 = min 9, <8 = max 7). Returns null when the text parses as none
// of these; "" clears both bounds.
export function parseCustody(
  raw: string,
): { min: string; max: string } | null {
  const s = raw.trim().replace(/\s+/g, "");
  if (!s) return { min: "", max: "" };
  let m = s.match(/^(\d{1,4})$/);
  if (m) return { min: m[1], max: m[1] };
  m = s.match(/^(\d{1,4})\+$/) ?? s.match(/^>=(\d{1,4})$/);
  if (m) return { min: m[1], max: "" };
  m = s.match(/^>(\d{1,4})$/);
  if (m) return { min: String(Number(m[1]) + 1), max: "" };
  m = s.match(/^<=(\d{1,4})$/);
  if (m) return { min: "", max: m[1] };
  m = s.match(/^<(\d{1,4})$/);
  if (m) return Number(m[1]) > 0 ? { min: "", max: String(Number(m[1]) - 1) } : null;
  m = s.match(/^(\d{1,4})-(\d{1,4})$/);
  if (m && Number(m[1]) <= Number(m[2])) return { min: m[1], max: m[2] };
  return null;
}

function custodyText(min: string, max: string): string {
  if (min && max) return min === max ? min : `${min}-${max}`;
  if (min) return `${min}+`;
  if (max) return `<=${max}`;
  return "";
}

function useDebouncedFilter(
  key: string,
  draft: string,
  current: string,
  patch: PatchFilter,
) {
  useEffect(() => {
    const value = draft.trim();
    if (value === current) return;
    const timer = window.setTimeout(
      () => patch(key, value),
      value ? FILTER_DEBOUNCE_MS : 0,
    );
    return () => window.clearTimeout(timer);
  }, [key, draft, current, patch]);
}

function SortableHeader({
  label,
  value,
  sort,
  order,
  onSort,
}: {
  label: string;
  value: NodeSort;
  sort: string;
  order: "asc" | "desc";
  onSort: (value: NodeSort) => void;
}) {
  const active = sort === value;
  return (
    <th
      aria-sort={
        active ? (order === "asc" ? "ascending" : "descending") : "none"
      }
    >
      <button
        className={active ? "sort-header active" : "sort-header"}
        type="button"
        onClick={() => onSort(value)}
      >
        {label}{" "}
        <span aria-hidden="true">
          {active ? (order === "asc" ? "↑" : "↓") : "↕"}
        </span>
      </button>
    </th>
  );
}

export default function NodesPage({ layer }: { layer: "el" | "cl" }) {
  const { network } = useNetwork();
  const [sp, setSp] = useSearchParams();
  const q = sp.get("q") ?? "";
  const client = sp.get("client") ?? "";
  const country = sp.get("country") ?? "";
  const protocol = sp.get("protocol") ?? "";
  const ipstack = sp.get("ipstack") ?? "";
  const hosting = sp.get("hosting") ?? "";
  const dialable = sp.get("dialable") ?? "";
  const cgcMin = layer === "cl" ? (sp.get("cgc_min") ?? "") : "";
  const cgcMax = layer === "cl" ? (sp.get("cgc_max") ?? "") : "";
  const ip = sp.get("ip") ?? "";
  const sortParam = sp.get("sort");
  const sort: NodeSort =
    sortParam === "client" || (sortParam === "cgc" && layer === "cl")
      ? sortParam
      : "last_seen";
  const orderParam = sp.get("order");
  const defaultOrder = sort === "client" ? "asc" : "desc";
  const order =
    orderParam === "asc" || orderParam === "desc" ? orderParam : defaultOrder;

  const [page, setPage] = useState(0);
  const [res, setRes] = useState<NodesResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [qDraft, setQDraft] = useState(q);
  const [ipDraft, setIPDraft] = useState(ip);
  const [clientDraft, setClientDraft] = useState(client);
  const [countryDraft, setCountryDraft] = useState(country);
  const custody = custodyText(cgcMin, cgcMax);
  const [custodyDraft, setCustodyDraft] = useState(custody);
  const filterKey = [
    network,
    q,
    ip,
    client,
    country,
    layer,
    protocol,
    ipstack,
    hosting,
    dialable,
    cgcMin,
    cgcMax,
    sort,
    order,
  ].join("\u0000");
  const requestedFilterKey = useRef(filterKey);

  const setSpRef = useRef(setSp);
  useEffect(() => {
    setSpRef.current = setSp;
  }, [setSp]);
  const patch = useCallback((k: string, v: string) => {
    setSpRef.current(
      (current) => {
        const next = new URLSearchParams(current);
        if (v) next.set(k, v);
        else next.delete(k);
        return next;
      },
      { replace: true },
    );
  }, []);

  const changeSort = (value: NodeSort) => {
    const next = new URLSearchParams(sp);
    if (value === sort) {
      next.set("order", order === "asc" ? "desc" : "asc");
    } else {
      if (value === "last_seen") next.delete("sort");
      else next.set("sort", value);
      next.delete("order");
    }
    setSp(next, { replace: true });
  };

  useEffect(
    () => setPage(0),
    [
      network,
      q,
      ip,
      client,
      country,
      layer,
      protocol,
      ipstack,
      hosting,
      dialable,
      cgcMin,
      cgcMax,
      sort,
      order,
    ],
  );

  useEffect(() => setQDraft(q), [q]);
  useEffect(() => setIPDraft(ip), [ip]);
  useEffect(() => setClientDraft(client), [client]);
  useEffect(() => setCountryDraft(country), [country]);
  useEffect(() => setCustodyDraft(custody), [custody]);

  useDebouncedFilter("q", qDraft, q, patch);
  useDebouncedFilter("ip", ipDraft, ip, patch);
  useDebouncedFilter("client", clientDraft, client, patch);
  useDebouncedFilter("country", countryDraft, country, patch);

  // One atomic update: two patch() calls race inside a React batch and the
  // second overwrites the first key from a stale base.
  const patchCustodyNow = useCallback((raw: string) => {
    const parsed = parseCustody(raw);
    if (!parsed) return;
    setSpRef.current(
      (current) => {
        const next = new URLSearchParams(current);
        if (parsed.min) next.set("cgc_min", parsed.min);
        else next.delete("cgc_min");
        if (parsed.max) next.set("cgc_max", parsed.max);
        else next.delete("cgc_max");
        return next;
      },
      { replace: true },
    );
  }, []);
  useEffect(() => {
    const parsed = parseCustody(custodyDraft);
    if (!parsed || custodyText(parsed.min, parsed.max) === custody) return;
    const timer = window.setTimeout(
      () => patchCustodyNow(custodyDraft),
      custodyDraft.trim() ? FILTER_DEBOUNCE_MS : 0,
    );
    return () => window.clearTimeout(timer);
  }, [custodyDraft, custody, patchCustodyNow]);

  useEffect(() => {
    if (requestedFilterKey.current !== filterKey) {
      requestedFilterKey.current = filterKey;
      // The reset effect will schedule the only request, at offset zero.
      if (page !== 0) return;
    }
    let live = true;
    setRes(null);
    setErr(null);
    fetchNodes({
      network,
      q,
      ip,
      client,
      country,
      layer,
      protocol,
      ipstack,
      hosting,
      dialable,
      cgc_min: cgcMin,
      cgc_max: cgcMax,
      sort,
      order,
      limit: PAGE,
      offset: page * PAGE,
    })
      .then((r) => live && (setRes(r), setErr(null)))
      .catch(
        (e) =>
          live &&
          (setRes(null), setErr(e instanceof Error ? e.message : String(e))),
      );
    return () => {
      live = false;
    };
  }, [
    network,
    q,
    ip,
    client,
    country,
    layer,
    protocol,
    ipstack,
    hosting,
    dialable,
    cgcMin,
    cgcMax,
    sort,
    order,
    page,
    filterKey,
  ]);

  useEffect(() => {
    if (!res || page === 0 || page * PAGE < res.total) return;
    setPage(Math.max(0, Math.ceil(res.total / PAGE) - 1));
  }, [res, page]);

  const total = res?.total ?? 0;
  const pages = Math.ceil(total / PAGE);
  const elTabParams = new URLSearchParams(sp);
  elTabParams.delete("cgc_min");
  elTabParams.delete("cgc_max");
  if (elTabParams.get("sort") === "cgc") {
    elTabParams.delete("sort");
    elTabParams.delete("order");
  }
  const elTabSearch = elTabParams.toString();

  return (
    <div className="page nodes">
      <div className="page-head">
        <h1>{layer === "cl" ? "Consensus" : "Execution"} identities</h1>
        <p className="sub">
          {num(total)} {layer === "cl" ? "consensus" : "execution"} identities
          on <b>{network}</b>
        </p>
      </div>

      <div className="tabs">
        <Link
          className={layer === "el" ? "tab active" : "tab"}
          to={{ pathname: "/nodes/execution", search: elTabSearch }}
        >
          Execution
        </Link>
        <Link
          className={layer === "cl" ? "tab active" : "tab"}
          to={{ pathname: "/nodes/consensus", search: sp.toString() }}
        >
          Consensus
        </Link>
      </div>

      <div className="filters">
        <input
          className="f-search"
          value={qDraft}
          onChange={(e) => setQDraft(e.target.value)}
          placeholder="node ID / enode / ENR"
          onBlur={() => patch("q", qDraft.trim())}
          onKeyDown={(e) => e.key === "Enter" && patch("q", qDraft.trim())}
        />
        <input
          className="f-ip"
          value={ipDraft}
          onChange={(e) => setIPDraft(e.target.value)}
          placeholder="IP address"
          onBlur={() => patch("ip", ipDraft.trim())}
          onKeyDown={(e) => e.key === "Enter" && patch("ip", ipDraft.trim())}
        />
        <input
          className="f-in"
          value={clientDraft}
          placeholder="client contains…"
          onChange={(e) => setClientDraft(e.target.value)}
          onBlur={() => patch("client", clientDraft.trim())}
          onKeyDown={(e) =>
            e.key === "Enter" && patch("client", clientDraft.trim())
          }
        />
        <input
          className="f-in"
          value={countryDraft}
          placeholder="country (US, DE…)"
          maxLength={2}
          onChange={(e) => setCountryDraft(e.target.value)}
          onBlur={() => patch("country", countryDraft.trim())}
          onKeyDown={(e) =>
            e.key === "Enter" && patch("country", countryDraft.trim())
          }
        />
        <select
          value={protocol}
          onChange={(e) => patch("protocol", e.target.value)}
        >
          <option value="">any protocol</option>
          <option value="v5">discv5</option>
          <option value="v4">discv4</option>
        </select>
        <select
          value={ipstack}
          onChange={(e) => patch("ipstack", e.target.value)}
        >
          <option value="">any IP stack</option>
          <option value="dual">dual-stack</option>
          <option value="ipv6">IPv6 only</option>
          <option value="ipv4">IPv4 only</option>
        </select>
        <select
          value={hosting}
          onChange={(e) => patch("hosting", e.target.value)}
        >
          <option value="">any host</option>
          <option value="yes">cloud / datacenter</option>
          <option value="no">known non-hosting</option>
        </select>
        <select
          value={dialable}
          onChange={(e) => patch("dialable", e.target.value)}
        >
          <option value="">any reachability</option>
          <option value="yes">dialable (TCP/QUIC)</option>
          <option value="no">discovery-only</option>
        </select>
        {layer === "cl" && (
          <input
            className="f-in"
            value={custodyDraft}
            placeholder="custody (8+, 4-8, 128)"
            title="Custody group count (cgc). Accepts an exact value (128 = supernode), a range (4-8), a minimum (8+ or >=8), a maximum (<=8), or strict bounds (>8, <8)."
            aria-invalid={parseCustody(custodyDraft) === null}
            onChange={(e) => setCustodyDraft(e.target.value)}
            onBlur={() => patchCustodyNow(custodyDraft)}
            onKeyDown={(e) => e.key === "Enter" && patchCustodyNow(custodyDraft)}
          />
        )}
      </div>

      {err && <div className="error">API unreachable: {err}</div>}

      <div className="table-wrap">
        <table className="nodes-table">
          <thead>
            <tr>
              <th>Node</th>
              <SortableHeader
                label="Client"
                value="client"
                sort={sort}
                order={order}
                onSort={changeSort}
              />
              <th>Version</th>
              <th>OS</th>
              <th>Lang</th>
              {layer === "cl" && (
                <SortableHeader
                  label="Custody"
                  value="cgc"
                  sort={sort}
                  order={order}
                  onSort={changeSort}
                />
              )}
              <th>Country</th>
              <th>IP</th>
              <th>Proto</th>
              <th>Reach</th>
              <SortableHeader
                label="Last discovered"
                value="last_seen"
                sort={sort}
                order={order}
                onSort={changeSort}
              />
            </tr>
          </thead>
          <tbody>
            {res?.nodes.map((n) => (
              <tr key={n.id}>
                <td>
                  <Link to={`/nodes/${n.id}`} className="mono">
                    {shortId(n.id, 12)}
                  </Link>
                </td>
                <td>
                  {n.client || "-"}{" "}
                  {n.client &&
                    n.fp_status !== "ok" &&
                    n.fp_status !== "stale" && (
                      <span
                        className="claimed-marker"
                        title="Client name claimed in the self-signed ENR; no successful client handshake yet"
                      >
                        ENR claim
                      </span>
                    )}
                </td>
                <td className="dim">{n.client_version || "-"}</td>
                <td className="dim">{n.os || "-"}</td>
                <td className="dim">{n.lang || "-"}</td>
                {layer === "cl" && (
                  <td>
                    {n.cgc_known ? (
                      n.cgc >= 128 ? (
                        <span className="supernode-tag">{n.cgc} ✨</span>
                      ) : (
                        n.cgc
                      )
                    ) : (
                      "-"
                    )}
                  </td>
                )}
                <td>{n.country || "-"}</td>
                <td className="mono dim">{n.ip || n.ip6 || "-"}</td>
                <td className="dim">
                  {[n.has_v5 && "v5", n.has_v4 && "v4"]
                    .filter(Boolean)
                    .join("/") || "-"}
                </td>
                <td>
                  <span
                    className={
                      n.dialable ? "reach reach-yes" : "reach reach-no"
                    }
                  >
                    {n.dialable ? "dialable" : "disc-only"}
                  </span>
                </td>
                <td className="dim">{relTime(n.last_seen)}</td>
              </tr>
            ))}
            {res && res.nodes.length === 0 && (
              <tr>
                <td colSpan={layer === "cl" ? 11 : 10} className="empty">
                  No identities match these filters.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {pages > 1 && (
        <div className="pager">
          <button disabled={page === 0} onClick={() => setPage((p) => p - 1)}>
            ← Prev
          </button>
          <span>
            Page {page + 1} of {pages}
          </span>
          <button
            disabled={page + 1 >= pages}
            onClick={() => setPage((p) => p + 1)}
          >
            Next →
          </button>
        </div>
      )}
    </div>
  );
}
