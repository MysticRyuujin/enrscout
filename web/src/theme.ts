const configuredNetworks = (import.meta.env.VITE_NETWORKS as string | undefined)
  ?.split(",")
  .map((s) => s.trim())
  .filter(Boolean);

export const NETWORKS: readonly string[] = configuredNetworks?.length
  ? configuredNetworks
  : ["mainnet", "hoodi", "sepolia"];

export const NETWORK_COLOR: Record<string, string> = {
  mainnet: "#3987e5",
  hoodi: "#199e70",
  sepolia: "#9085e9",
};

export const ACCENT = "#3987e5";

export const CATEGORICAL = [
  "#3987e5",
  "#199e70",
  "#d55181",
  "#c98500",
  "#9085e9",
  "#d95926",
  "#008300",
  "#e66767",
];
export const OTHER_COLOR = "#5f6b7e";

// Stable client colors shared by every client visualization. Only recognized L1
// clients get a color; anything else (crawlers, tooling, garbage) falls to
// OTHER_COLOR so the legend never mints a swatch per stranger. Keep the key set in
// sync with clientname.recognized in internal/clientname/clientname.go.
export const CLIENT_COLOR: Readonly<Record<string, string>> = {
  geth: "#5B8FF9",
  nethermind: "#E65A9E",
  besu: "#36B5A7",
  erigon: "#55B86A",
  reth: "#F09A4A",
  ethrex: "#E46D5E",
  ethereumjs: "#C79A3D",
  lighthouse: "#E45D6F",
  prysm: "#A47BE8",
  teku: "#32A6D6",
  nimbus: "#E7C84B",
  lodestar: "#49C59A",
  grandine: "#F0784F",
  caplin: "#82B35A",
};

export function clientColor(name: string): string {
  return CLIENT_COLOR[name.trim().toLowerCase()] ?? OTHER_COLOR;
}

export function hexRGB(hex: string): [number, number, number] {
  const h = hex.replace("#", "");
  return [
    parseInt(h.slice(0, 2), 16),
    parseInt(h.slice(2, 4), 16),
    parseInt(h.slice(4, 6), 16),
  ];
}

export function networkRGB(network: string): [number, number, number] {
  return hexRGB(NETWORK_COLOR[network] || "#8a97ab");
}

export function num(n: number): string {
  return n.toLocaleString();
}

export function shortId(id: string, n = 10): string {
  return id.length > n ? id.slice(0, n) + "…" : id;
}

export function relTime(unixSec: number): string {
  if (!unixSec) return "-";
  return durationAgo(Date.now() / 1000 - unixSec);
}

export function durationAgo(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function topN(
  m: Record<string, number> | undefined | null,
  n: number,
): [string, number][] {
  if (!m) return [];
  return Object.entries(m)
    .sort((a, b) => b[1] - a[1])
    .slice(0, n);
}
