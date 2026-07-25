import { createContext, useContext } from "react";

export interface NetworkCtx {
  network: string;
  networks: readonly string[];
  setNetwork: (n: string) => void;
}

export const NetworkContext = createContext<NetworkCtx>({
  network: "mainnet",
  networks: ["mainnet"],
  setNetwork: () => {},
});

export function useNetwork(): NetworkCtx {
  return useContext(NetworkContext);
}
