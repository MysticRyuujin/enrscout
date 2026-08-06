import { useState } from "react";
import { NavLink, useLocation, useNavigate } from "react-router";
import { useNetwork } from "../network";

function BrandMark() {
  return (
    <svg className="brand-mark" viewBox="0 0 32 32" aria-hidden="true">
      <mask id="brand-mark-holes">
        <rect width="32" height="32" fill="#fff" />
        <g fill="none" stroke="#000">
          <circle cx="16" cy="16" r="5" strokeWidth="1.4" />
          <g strokeWidth="1.6">
            <circle cx="21" cy="7.3" r="3.8" />
            <circle cx="6" cy="16" r="3.8" />
            <circle cx="21" cy="24.7" r="3.8" />
          </g>
        </g>
        <g fill="#000">
          <circle cx="21" cy="7.3" r="1.5" />
          <circle cx="6" cy="16" r="1.5" />
          <circle cx="21" cy="24.7" r="1.5" />
        </g>
      </mask>
      <g mask="url(#brand-mark-holes)">
        <circle
          cx="16"
          cy="16"
          r="10.4"
          fill="none"
          stroke="#3987e5"
          strokeWidth="2.2"
        />
        <circle cx="16" cy="16" r="5" fill="#3987e5" />
        <circle cx="16" cy="16" r="2.3" fill="#d55181" />
        <circle cx="21" cy="7.3" r="3.8" fill="#3987e5" />
        <circle cx="6" cy="16" r="3.8" fill="#199e70" />
        <circle cx="21" cy="24.7" r="3.8" fill="#9085e9" />
      </g>
    </svg>
  );
}

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
          <BrandMark /> ENRScout
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
