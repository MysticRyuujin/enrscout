import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router";
import { fetchForks } from "../api";
import { useNetwork } from "../network";
import { whileVisible } from "../theme";
import type { Notice } from "../types";

// Operator text is rendered as text and links only: the file is trusted, but it is edited by hand
// on a live site, so nothing in it is ever interpreted as markup.
function paragraphs(body?: string): string[] {
  return (body ?? "")
    .split(/\n\s*\n/)
    .map((p) => p.replace(/\s+/g, " ").trim())
    .filter(Boolean);
}

export function NoticeCard({ notice }: { notice: Notice }) {
  return (
    <section
      className={`notice notice-${notice.severity}`}
      role={notice.severity === "critical" ? "alert" : "note"}
    >
      <h2 className="notice-title">{notice.title}</h2>
      {paragraphs(notice.body).map((p) => (
        <p key={p}>{p}</p>
      ))}
      {notice.links && notice.links.length > 0 && (
        <ul className="notice-links">
          {notice.links.map((l) => (
            <li key={l.url}>
              <a href={l.url} target="_blank" rel="noreferrer">
                {l.label}
              </a>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export default function Notices({ notices }: { notices?: Notice[] }) {
  if (!notices?.length) return null;
  return (
    <>
      {notices.map((n) => (
        <NoticeCard key={n.title} notice={n} />
      ))}
    </>
  );
}

function dismissedKey(network: string, n: Notice): string {
  return `notice:${network}:${n.title}`;
}

function readDismissed(key: string): boolean {
  try {
    return sessionStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

export function NoticeStrip() {
  const { network } = useNetwork();
  const { pathname } = useLocation();
  const [notice, setNotice] = useState<Notice | null>(null);
  const [dismissed, setDismissed] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setNotice(null);
    const load = () =>
      fetchForks(network)
        .then((d) => live && setNotice(d.notices?.[0] ?? null))
        .catch(() => {});
    void load();
    const timer = window.setInterval(whileVisible(load), 5 * 60_000);
    return () => {
      live = false;
      window.clearInterval(timer);
    };
  }, [network]);

  if (!notice || pathname.startsWith("/forks")) return null;
  const key = dismissedKey(network, notice);
  if (dismissed === key || readDismissed(key)) return null;
  const dismiss = () => {
    try {
      sessionStorage.setItem(key, "1");
    } catch {
      // Without storage the strip still hides for this page view.
    }
    setDismissed(key);
  };
  return (
    <div className={`notice-strip notice-${notice.severity}`} role="note">
      <span className="notice-strip-title">{notice.title}</span>
      <Link to={`/forks?network=${encodeURIComponent(network)}`}>Details</Link>
      <button
        className="notice-strip-close"
        onClick={dismiss}
        aria-label="Dismiss notice"
      >
        ×
      </button>
    </div>
  );
}
