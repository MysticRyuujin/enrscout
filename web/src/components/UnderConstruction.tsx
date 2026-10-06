import { useEffect, useState } from "react";
import { Link } from "react-router";
import { fetchForks } from "../api";
import { useNetwork } from "../network";
import { whileVisible } from "../theme";

const WINDOW_S = 600;
const NEAR_S = WINDOW_S + 120;

interface Activation {
  name: string;
  at: number;
}

function clock(s: number): string {
  const m = Math.floor(s / 60);
  return `${String(m).padStart(2, "0")}:${String(Math.floor(s % 60)).padStart(2, "0")}`;
}

function dismissedKey(network: string, a: Activation): string {
  return `under-construction:${network}:${a.at}`;
}

function readDismissed(key: string): boolean {
  try {
    return sessionStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

export default function UnderConstruction() {
  const { network } = useNetwork();
  const [activation, setActivation] = useState<Activation | null>(null);
  const [now, setNow] = useState(() => Date.now() / 1000);
  const [dismissed, setDismissed] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setActivation(null);
    const load = () =>
      fetchForks(network)
        .then((d) => {
          if (!live) return;
          const at =
            d.fork.el?.time ??
            (d.fork.cl ? Date.parse(d.fork.cl.time) / 1000 : undefined);
          setActivation(
            d.phase === "none" || at === undefined
              ? null
              : { name: d.fork.name, at },
          );
        })
        .catch(() => {});
    void load();
    const timer = window.setInterval(whileVisible(load), 5 * 60_000);
    return () => {
      live = false;
      window.clearInterval(timer);
    };
  }, [network]);

  const near = activation !== null && Math.abs(now - activation.at) <= NEAR_S;
  useEffect(() => {
    const timer = window.setInterval(
      () => setNow(Date.now() / 1000),
      near ? 1000 : 30_000,
    );
    return () => window.clearInterval(timer);
  }, [near]);

  if (!activation || Math.abs(now - activation.at) > WINDOW_S) return null;
  const key = dismissedKey(network, activation);
  if (dismissed === key || readDismissed(key)) return null;

  const before = now < activation.at;
  const close = () => {
    setDismissed(key);
    try {
      sessionStorage.setItem(key, "1");
    } catch {
      return;
    }
  };

  return (
    <section className="uc" aria-label={`${activation.name} activation`}>
      <div className="uc-tape" aria-hidden="true" />
      <div className="uc-body">
        <span className="uc-sign" aria-hidden="true">
          🚧
        </span>
        <div className="uc-text">
          <strong>
            {activation.name} is{" "}
            <span className="uc-blink">
              {before ? "UNDER CONSTRUCTION" : "FRESHLY POURED"}
            </span>{" "}
            on <span className="net-name">{network}</span>
          </strong>
          <span className="uc-clock">
            {before
              ? `T-${clock(activation.at - now)}: hard hats on, the crew is bolting on the new fork.`
              : `T+${clock(now - activation.at)}: pardon our dust while the crew re-checks every node.`}{" "}
            <Link to="/forks">Watch the upgrade live</Link>
          </span>
          <span className="uc-retro">
            Best viewed in Netscape Navigator 4.0 at 800×600
          </span>
        </div>
        <span className="uc-sign" aria-hidden="true">
          🏗️
        </span>
        <button className="uc-close" onClick={close} aria-label="Dismiss">
          ×
        </button>
      </div>
      <div className="uc-site" aria-hidden="true">
        <span className="uc-worker">👷</span>
        <span className="uc-worker uc-worker-2">👷‍♀️</span>
        <span className="uc-cone">🚧</span>
      </div>
      <div className="uc-tape" aria-hidden="true" />
    </section>
  );
}
