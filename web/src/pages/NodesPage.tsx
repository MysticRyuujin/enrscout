import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { fetchNodes } from "../api";
import { useNetwork } from "../network";
import { layerName, num, relTime, shortId, SUPERNODE_CGC } from "../theme";
import type { NodeQuery, NodesResult } from "../types";

const PAGE = 50;
const FILTER_DEBOUNCE_MS = 250;

type PatchFilter = (key: string, value: string, clear?: string[]) => void;
type NodeSort = "last_seen" | "client" | "cgc";

// Accepted custody expressions: "128" (exact), "4-8" (range), "8+" / ">=8"
// (at least), "<=8" (at most), and strict ">8" / "<8", exact in the integer
// domain (>8 = min 9, <8 = max 7). Returns null when the text parses as none
// of these; "" clears both bounds.
export function parseCustody(raw: string): { min: string; max: string } | null {
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
  if (m)
    return Number(m[1]) > 0 ? { min: "", max: String(Number(m[1]) - 1) } : null;
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

// A text filter that edits a local draft and commits it to the URL after a pause, on blur, or on Enter.
function DebouncedParamInput({
  param,
  value,
  patch,
  ...props
}: {
  param: string;
  value: string;
  patch: PatchFilter;
  className: string;
  placeholder: string;
  maxLength?: number;
}) {
  const [draft, setDraft] = useState(value);
  useEffect(() => setDraft(value), [value]);
  useEffect(() => {
    const next = draft.trim();
    if (next === value) return;
    const timer = window.setTimeout(
      () => patch(param, next),
      next ? FILTER_DEBOUNCE_MS : 0,
    );
    return () => window.clearTimeout(timer);
  }, [param, draft, value, patch]);
  return (
    <input
      {...props}
      value={draft}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => patch(param, draft.trim())}
      onKeyDown={(e) => e.key === "Enter" && patch(param, draft.trim())}
    />
  );
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
  const param = (k: string) => sp.get(k) ?? "";
  const sortParam = sp.get("sort");
  const sort: NodeSort =
    sortParam === "client" || (sortParam === "cgc" && layer === "cl")
      ? sortParam
      : "last_seen";
  const orderParam = sp.get("order");
  const defaultOrder = sort === "client" ? "asc" : "desc";
  const order =
    orderParam === "asc" || orderParam === "desc" ? orderParam : defaultOrder;
  const query: NodeQuery = {
    network,
    layer,
    q: param("q"),
    ip: param("ip"),
    client: param("client"),
    client_exact: param("client_exact"),
    country: param("country"),
    protocol: param("protocol"),
    ipstack: param("ipstack"),
    hosting: param("hosting"),
    dialable: param("dialable"),
    identified: param("identified"),
    sync: param("sync"),
    readiness: param("readiness"),
    fork: param("fork"),
    cgc_min: layer === "cl" ? param("cgc_min") : "",
    cgc_max: layer === "cl" ? param("cgc_max") : "",
    sort,
    order,
  };
  // Every effect keys on this one string, so a new filter cannot be missed by one of them.
  const filterKey = JSON.stringify(query);
  const cgcMin = query.cgc_min ?? "";
  const cgcMax = query.cgc_max ?? "";

  // A page belongs to the filter it was chosen under; any filter change resets it to zero. The reset
  // is stored, not only derived, so returning to an earlier filter does not restore its old page.
  const [pageState, setPageState] = useState({ key: filterKey, page: 0 });
  if (pageState.key !== filterKey) setPageState({ key: filterKey, page: 0 });
  const page = pageState.key === filterKey ? pageState.page : 0;
  const setPage = (next: number) =>
    setPageState({ key: filterKey, page: next });
  const [res, setRes] = useState<NodesResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const custody = custodyText(cgcMin, cgcMax);
  const [custodyDraft, setCustodyDraft] = useState(custody);

  const setSpRef = useRef(setSp);
  useEffect(() => {
    setSpRef.current = setSp;
  }, [setSp]);
  const patch = useCallback((k: string, v: string, clear: string[] = []) => {
    setSpRef.current(
      (current) => {
        const next = new URLSearchParams(current);
        if ((next.get(k) ?? "") === v) return current;
        if (v) next.set(k, v);
        else next.delete(k);
        for (const key of clear) next.delete(key);
        return next;
      },
      { replace: true },
    );
  }, []);
  const patchClient = useCallback(
    (k: string, v: string) => patch(k, v, ["client_exact"]),
    [patch],
  );

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

  useEffect(() => setCustodyDraft(custody), [custody]);

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
    let live = true;
    setRes(null);
    setErr(null);
    fetchNodes({ ...query, limit: PAGE, offset: page * PAGE })
      .then((r) => live && (setRes(r), setErr(null)))
      .catch(
        (e) =>
          live &&
          (setRes(null), setErr(e instanceof Error ? e.message : String(e))),
      );
    return () => {
      live = false;
    };
    // query is a pure function of filterKey.
  }, [filterKey, page]);

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
        <h1>{layerName(layer)} identities</h1>
        <p className="sub">
          {num(total)} {layerName(layer).toLowerCase()} identities on{" "}
          <b>{network}</b>
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
        <DebouncedParamInput
          param="q"
          value={query.q ?? ""}
          patch={patch}
          className="f-search"
          placeholder="node ID / enode / ENR"
        />
        <DebouncedParamInput
          param="ip"
          value={query.ip ?? ""}
          patch={patch}
          className="f-ip"
          placeholder="IP address"
        />
        <DebouncedParamInput
          param="client"
          value={query.client ?? ""}
          patch={patchClient}
          className="f-in"
          placeholder="client contains…"
        />
        <DebouncedParamInput
          param="country"
          value={query.country ?? ""}
          patch={patch}
          className="f-in"
          placeholder="country (US, DE…)"
          maxLength={2}
        />
        <select
          value={query.protocol}
          onChange={(e) => patch("protocol", e.target.value)}
        >
          <option value="">any protocol</option>
          <option value="v5">discv5</option>
          <option value="v4">discv4</option>
        </select>
        <select
          value={query.ipstack}
          onChange={(e) => patch("ipstack", e.target.value)}
        >
          <option value="">any IP stack</option>
          <option value="dual">dual-stack</option>
          <option value="ipv6">IPv6 only</option>
          <option value="ipv4">IPv4 only</option>
        </select>
        <select
          value={query.hosting}
          onChange={(e) => patch("hosting", e.target.value)}
        >
          <option value="">any host</option>
          <option value="yes">cloud / datacenter</option>
          <option value="no">known non-hosting</option>
        </select>
        <select
          value={query.dialable}
          onChange={(e) => patch("dialable", e.target.value)}
        >
          <option value="">any reachability</option>
          <option value="yes">dialable (TCP/QUIC)</option>
          <option value="no">discovery-only</option>
        </select>
        <select
          value={query.identified}
          onChange={(e) => patch("identified", e.target.value)}
          title="Recently identified: a verified client handshake in the last 7 days. This is the population the Overview client charts count; the default also lists ENR-claimed names and older identifications."
        >
          <option value="">any identification</option>
          <option value="recent">identified in last 7 days</option>
        </select>
        <select
          value={query.sync}
          onChange={(e) => patch("sync", e.target.value)}
          title="Sync state compares the head a node reported in its last Status with the heads other peers reported in the same ten minutes. It is peer-reported, not a trusted chain head."
        >
          <option value="">any sync state</option>
          <option value="synced">synced</option>
          <option value="lagging">lagging</option>
          <option value="unknown">sync unknown</option>
        </select>
        <select
          value={query.fork || (query.readiness ? "all" : "current")}
          onChange={(e) => patch("fork", e.target.value)}
        >
          <option value="current">current fork</option>
          <option value="stale">older fork</option>
          <option value="all">any fork</option>
        </select>
        <select
          value={query.readiness}
          onChange={(e) => patch("readiness", e.target.value)}
          title="Readiness for the next scheduled fork, from the fork schedule the node itself advertises. See the Forks page."
        >
          <option value="">any fork readiness</option>
          <option value="ready">fork scheduled / upgraded</option>
          <option value="not_ready">not scheduled / left behind</option>
          <option value="mismatch">other schedule</option>
          <option value="unknown">schedule unknown</option>
          <option value="stale">older fork</option>
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
            onKeyDown={(e) =>
              e.key === "Enter" && patchCustodyNow(custodyDraft)
            }
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
                      n.cgc >= SUPERNODE_CGC ? (
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
          <button disabled={page === 0} onClick={() => setPage(page - 1)}>
            ← Prev
          </button>
          <span>
            Page {page + 1} of {pages}
          </span>
          <button
            disabled={page + 1 >= pages}
            onClick={() => setPage(page + 1)}
          >
            Next →
          </button>
        </div>
      )}
    </div>
  );
}
