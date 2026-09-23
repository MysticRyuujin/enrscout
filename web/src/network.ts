import { createContext, useContext } from "react";

import type { Meta } from "./types";

export interface NetworkCtx {
  network: string;
  networks: readonly string[];
  setNetwork: (n: string) => void;
  // meta is fetched once by App; null until it arrives or if the API is unreachable.
  meta: Meta | null;
}

export const NetworkContext = createContext<NetworkCtx>({
  network: "mainnet",
  networks: ["mainnet"],
  setNetwork: () => {},
  meta: null,
});

export function useNetwork(): NetworkCtx {
  return useContext(NetworkContext);
}
