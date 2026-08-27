import { num } from "../theme";

export type TileFilter = "only" | "hide" | null;

export interface Tile {
  label: string;
  value: number;
  hint?: string;
  filter?: TileFilter;
  onFilter?: () => void;
  title?: string;
  stateLabels?: { only: string; hide: string };
  stateLabelOverride?: string;
}

export default function StatTiles({ tiles }: { tiles: Tile[] }) {
  return (
    <div className="tiles">
      {tiles.map((t) =>
        t.onFilter ? (
          <button
            className={`tile tile-btn${t.filter ? ` tile-${t.filter}` : ""}`}
            key={t.label}
            onClick={t.onFilter}
            aria-pressed={t.filter === "hide" ? "mixed" : t.filter === "only"}
            title={t.title ?? "Click to filter the map: only → hidden → off"}
          >
            <span className="tile-num">{num(t.value)}</span>
            <span className="tile-lbl">{t.label}</span>
            {t.filter ? (
              <span className="tile-state">
                {t.stateLabelOverride ??
                  (t.filter === "only"
                    ? (t.stateLabels?.only ?? "map: only these")
                    : (t.stateLabels?.hide ?? "map: hidden"))}
              </span>
            ) : (
              <span className="tile-hint" aria-hidden={!t.hint}>
                {t.hint ?? "\u00A0"}
              </span>
            )}
          </button>
        ) : (
          <div className="tile" key={t.label}>
            <span className="tile-num">{num(t.value)}</span>
            <span className="tile-lbl">{t.label}</span>
            <span className="tile-hint" aria-hidden={!t.hint}>
              {t.hint ?? "\u00A0"}
            </span>
          </div>
        ),
      )}
    </div>
  );
}
