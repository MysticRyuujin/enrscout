import { useState } from "react";
import { NavLink, useLocation, useNavigate } from "react-router";
import { useNetwork } from "../network";

export default function Nav() {
  const { network, networks, setNetwork } = useNetwork();
  const [q, setQ] = useState("");
  const navigate = useNavigate();
  const nodesActive = useLocation().pathname.startsWith("/nodes");

  const search = (e: React.FormEvent) => {
    e.preventDefault();
    const v = q.trim();
    if (v) navigate(`/nodes/execution?q=${encodeURIComponent(v)}`);
  };

  return (
    <nav className="nav">
      <div className="nav-brand">
        <NavLink to="/" className="brand">
          <span className="brand-mark">◈</span> ENRScout
        </NavLink>
      </div>

      <div className="nav-links">
        <NavLink
          to="/"
          end
          className={({ isActive }) => (isActive ? "link active" : "link")}
        >
          Overview
        </NavLink>
        <NavLink
          to="/nodes/execution"
          className={nodesActive ? "link active" : "link"}
        >
          Nodes
        </NavLink>
        <NavLink
          to="/about"
          className={({ isActive }) => (isActive ? "link active" : "link")}
        >
          About
        </NavLink>
      </div>

      <form className="nav-search" onSubmit={search}>
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Search IP / enode / node id"
          spellCheck={false}
        />
      </form>

      <div className="nav-networks">
        {networks.map((n) => (
          <button
            key={n}
            className={network === n ? `net-pill ${n} active` : "net-pill"}
            onClick={() => setNetwork(n)}
          >
            {n}
          </button>
        ))}
      </div>
    </nav>
  );
}
