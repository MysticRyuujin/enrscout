import type {
  CompactMap,
  Meta,
  Node,
  NodeQuery,
  NodesResult,
  Stats,
} from "./types";

const BASE =
  (import.meta.env.VITE_API_BASE as string | undefined)?.replace(/\/$/, "") ||
  "";

export class ApiError extends Error {
  constructor(
    path: string,
    readonly status: number,
  ) {
    super(`${path}: ${status}`);
  }
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`);
  if (!res.ok) throw new ApiError(path, res.status);
  return res.json() as Promise<T>;
}

export function fetchStats(network: string, client?: string): Promise<Stats> {
  const p = new URLSearchParams();
  if (network) p.set("network", network);
  if (client) p.set("client", client);
  const qs = p.toString();
  return get<Stats>(`/api/v1/stats${qs ? `?${qs}` : ""}`);
}

export function fetchMap(network: string): Promise<CompactMap> {
  const p = new URLSearchParams({ format: "compact" });
  if (network) p.set("network", network);
  return get<CompactMap>(`/api/v1/map?${p}`);
}

export function fetchMeta(): Promise<Meta> {
  return get<Meta>("/api/v1/meta");
}

export function fetchNode(key: string): Promise<Node> {
  return get<Node>(`/api/v1/nodes/${encodeURIComponent(key)}`);
}

export function fetchNodes(query: NodeQuery): Promise<NodesResult> {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v !== undefined && v !== "") p.set(k, String(v));
  }
  const qs = p.toString();
  return get<NodesResult>(`/api/v1/nodes${qs ? `?${qs}` : ""}`);
}
