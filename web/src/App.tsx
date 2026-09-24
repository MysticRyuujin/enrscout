import { lazy, Suspense, useEffect, useState } from "react";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useSearchParams,
} from "react-router";
import Nav from "./components/Nav";
import { NetworkContext } from "./network";
import { NETWORKS } from "./theme";
import { fetchMeta } from "./api";
import type { Meta } from "./types";

const Overview = lazy(() => import("./pages/Overview"));
const NodesPage = lazy(() => import("./pages/NodesPage"));
const NodeDetail = lazy(() => import("./pages/NodeDetail"));
const About = lazy(() => import("./pages/About"));
const Forks = lazy(() => import("./pages/Forks"));

function NodesRedirect() {
  const { search } = useLocation();
  return <Navigate to={{ pathname: "/nodes/execution", search }} replace />;
}

function withNetwork(n: string) {
  return (prev: URLSearchParams) => {
    const next = new URLSearchParams(prev);
    next.set("network", n);
    return next;
  };
}

function readStoredNetwork(): string {
  const saved = localStorage.getItem("network");
  return saved && NETWORKS.includes(saved) ? saved : NETWORKS[0];
}

export default function App() {
  const [networks, setNetworks] = useState<readonly string[]>(NETWORKS);
  const [meta, setMeta] = useState<Meta | null>(null);
  const [networksReady, setNetworksReady] = useState(false);
  const [searchParams, setSearchParams] = useSearchParams();
  const networkParam = searchParams.get("network");
  const [network, setNetworkState] = useState<string>(
    () => networkParam || readStoredNetwork(),
  );

  useEffect(() => {
    let live = true;
    fetchMeta()
      .then((m) => {
        if (!live) return;
        setMeta(m);
        if (m.networks?.length) setNetworks(m.networks);
      })
      .catch(() => {})
      .finally(() => live && setNetworksReady(true));
    return () => {
      live = false;
    };
  }, []);

  useEffect(() => {
    // Preserve a possibly dynamic devnet parameter until /meta provides the
    // authoritative network list. With no parameter, expose the local fallback
    // immediately so the initial URL is shareable.
    if (!networksReady) {
      if (!networkParam) {
        setSearchParams(withNetwork(network), { replace: true });
      }
      return;
    }

    if (networkParam && networks.includes(networkParam)) {
      if (networkParam !== network) {
        setNetworkState(networkParam);
        localStorage.setItem("network", networkParam);
      }
      return;
    }

    const fallback = networks.includes(network) ? network : networks[0];
    if (fallback !== network) setNetworkState(fallback);
    localStorage.setItem("network", fallback);
    setSearchParams(withNetwork(fallback), { replace: true });
  }, [network, networkParam, networks, networksReady, setSearchParams]);

  const set = (n: string) => {
    setNetworkState(n);
    localStorage.setItem("network", n);
    setSearchParams(withNetwork(n), { replace: true });
  };

  return (
    <NetworkContext.Provider
      value={{ network, networks, setNetwork: set, meta }}
    >
      <div className="shell">
        <Nav />
        <main className="content">
          <Suspense fallback={<div className="loading">Loading…</div>}>
            {networksReady && networks.includes(network) ? (
              <Routes>
                <Route path="/" element={<Overview />} />
                <Route path="/nodes" element={<NodesRedirect />} />
                <Route
                  path="/nodes/execution"
                  element={<NodesPage layer="el" />}
                />
                <Route
                  path="/nodes/consensus"
                  element={<NodesPage layer="cl" />}
                />
                <Route path="/nodes/:key" element={<NodeDetail />} />
                <Route path="/forks" element={<Forks />} />
                <Route path="/about" element={<About />} />
              </Routes>
            ) : (
              <div className="loading">Loading…</div>
            )}
          </Suspense>
        </main>
      </div>
    </NetworkContext.Provider>
  );
}
