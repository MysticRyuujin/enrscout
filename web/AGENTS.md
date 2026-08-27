# AGENTS.md: web explorer

Frontend-specific guidance for the `web/` SPA (React + deck.gl, built with Vite). See
the repository-root AGENTS.md for architecture and cross-cutting constraints.

## Non-obvious constraints

- **deck.gl tooltips.** Prefer `getTooltip` `text` (innerText). The `html` field renders
  as innerHTML; never put peer-controlled strings there without escaping.
- **The brand mark exists twice.** `BrandMark` in `src/components/Nav.tsx` and
  `public/favicon.svg`. A static favicon cannot import from the bundle, so the geometry
  is copied; change both. Its dark separators are cut with an SVG mask rather than
  painted, because the nav bar is translucent over scrolling content.
- Network accent color follows the selected network in tiles/headers; the map colors
  points **by client** with one stable assignment ranked by within-layer share, and
  donut _category_ colors are fixed (validated categorical palette in `web/src/theme.ts`).
