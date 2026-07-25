# AGENTS.md — web explorer

Frontend-specific guidance for the `web/` SPA (React + deck.gl, built with Vite). See
the repository-root AGENTS.md for architecture and cross-cutting constraints.

## Non-obvious constraints

- **deck.gl tooltips:** prefer `getTooltip` `text` (innerText). The `html` field renders
  as innerHTML — never put peer-controlled strings there without escaping.
- Network accent color follows the selected network in tiles/headers; the map colors
  points **by client** with one stable assignment ranked by within-layer share, and
  donut _category_ colors are fixed (validated categorical palette in `web/src/theme.ts`).
