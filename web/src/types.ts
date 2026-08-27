export interface Stats {
  fork_evaluated_at: string;
  snapshot_generated_at?: string;
  snapshot_age_seconds: number;
  fingerprint_window_seconds: number;
  el_identified: number;
  cl_identified: number;
  warming_up: boolean;
  warmup_ends_at?: string;
  warmup_basis?: string;
  warmup_reasons?: string[];
  total: number;
  execution: number;
  execution_stale: number;
  consensus_stale: number;
  el_identified_stale: number;
  cl_identified_stale: number;
  membership_verified: number;
  membership_claimed: number;
  consensus: number;
  discv4: number;
  discv5: number;
  geolocated: number;
  ipv6: number;
  dualstack: number;
  hosting: number;
  dialable: number;
  by_network: Record<string, number>;
  by_client: Record<string, number>;
  by_client_el: Record<string, number>;
  by_client_cl: Record<string, number>;
  by_direction_el: Record<string, number>;
  by_direction_cl: Record<string, number>;
  by_country: Record<string, number>;
  by_org: Record<string, number>;
  by_os: Record<string, number>;
  by_layer: Record<string, number>;
  by_version: Record<string, number>;
}

export interface Node {
  id: string;
  enode: string;
  enr: string;
  seq: string;
  ip: string;
  ip6: string;
  tcp: number;
  udp: number;
  tcp6: number;
  udp6: number;
  quic: number;
  quic6: number;
  network: string;
  fork_hash: string;
  fork_next: string;
  fork_compatible: boolean;
  layer: string;
  cgc: number;
  cgc_known: boolean;
  has_v4: boolean;
  has_v5: boolean;
  score: number;
  first_seen: number;
  last_seen: number;
  last_check: number;
  last_resolved: number;
  client: string;
  client_version: string;
  os: string;
  lang: string;
  capabilities: string;
  country: string;
  city: string;
  subdivision: string;
  lat: number;
  lon: number;
  asn: number;
  org: string;
  hosting: boolean;
  hosting_known: boolean;
  fp_status: string;
  fingerprint_at: number;
  membership_source: string;
  membership_verified_at: number;
  fork_source: string;
  fork_observed_at: number;
  fp_direction: string;
  dialable: boolean;
  pinned: boolean;
  geolocated: boolean;
  geo_accuracy_radius_km: number;
}

export interface NodesResult {
  total: number;
  count: number;
  nodes: Node[];
}

export interface Meta {
  nodes: number;
  networks: string[];
  generated_at?: string;
  age_seconds?: number;
  schema_version?: number;
  run_id?: string;
  source_revision?: string;
  source_url?: string;
}

export type MapPoint = [
  id: string,
  longitude: number,
  latitude: number,
  client: string,
  country: string,
  city: string,
  layer: string,
  hosting: 0 | 1,
  ipv6: 0 | 1,
  verified: 0 | 1,
  accuracyKM: number,
  subdivision?: string,
  cgc?: number,
];

export const pointClient = (p: MapPoint) => p[3];
export const pointLayer = (p: MapPoint) => p[6];
export const pointHosting = (p: MapPoint) => p[7] === 1;
export const pointIPv6 = (p: MapPoint) => p[8] === 1;
export const pointVerified = (p: MapPoint) => p[9] === 1;
export const pointAccuracyKM = (p: MapPoint) => p[10] ?? 0;
export const pointSubdivision = (p: MapPoint) => p[11] ?? "";
export const pointCGC = (p: MapPoint) => p[12] ?? 0;

export interface CompactMap {
  points: MapPoint[];
  returned?: number;
  total?: number;
  truncated?: boolean;
}

export interface NodeQuery {
  network?: string;
  client?: string;
  country?: string;
  layer?: string;
  protocol?: string;
  ipstack?: string;
  hosting?: string;
  dialable?: string;
  fork?: string;
  ip?: string;
  q?: string;
  cgc_min?: string;
  cgc_max?: string;
  sort?: string;
  order?: "asc" | "desc";
  limit?: number;
  offset?: number;
}
