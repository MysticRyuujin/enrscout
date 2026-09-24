# Design contract

Rules the web explorer must hold at every viewport, and the local procedure that checks them.
There is no design reviewer on this project, so the procedure is the review: the agent runs the
check, reads every changed screenshot against these rules, and fixes what breaks them before it
commits. Nothing here runs in CI.

## Viewports

| Name    | Width | Height | Stands for                       |
| ------- | ----- | ------ | -------------------------------- |
| phone   | 390   | 844    | a current phone in portrait      |
| tablet  | 820   | 1180   | a tablet, or a narrow window     |
| laptop  | 1280  | 800    | a laptop, or a browser side pane |
| desktop | 1920  | 1080   | a desktop monitor                |

Every page is checked at every viewport: Overview, Nodes (execution and consensus), node detail,
Forks, and About.

## Rules

Checked by `web/e2e/design.mjs`. A **fail** stops the commit. A **warn** is a taste finding: the
agent looks at the screenshot and either fixes it or states in the commit message why it stays.

| Rule                 | Level | Meaning                                                                                                                 |
| -------------------- | ----- | ----------------------------------------------------------------------------------------------------------------------- |
| `noHorizontalScroll` | fail  | The page never scrolls sideways. Long identifiers wrap or scroll inside their own box.                                  |
| `selectLabelsFit`    | fail  | The chosen option of every select fits inside the box. Shorten the label or widen the column; never let it clip.       |
| `gridsAreEven`       | fail  | Controls in one grid (`.filters`, `.tiles`) share one width, and one height per row. No control stretches on its own. |
| `noOrphanRow`        | warn  | The last row of a multi-row grid does not hold a single control. An auto-fill grid cannot avoid this at every width.  |
| `readableText`       | warn  | No visible text is under 11px.                                                                                          |
| `tapTargets`         | warn  | On the phone viewport, buttons, selects, and inputs are at least 28px tall.                                             |
| `no page errors`     | fail  | No uncaught JavaScript error on any page.                                                                               |

Rules the check cannot measure, applied by eye on the screenshots:

- One idea per row. Text inputs, then selects, then anything layer-specific. Do not interleave.
- A row of tiles reads as one line: same height, same label style, numbers aligned.
- Headings use the existing type scale; nothing on a page is larger than its `h1`.
- Colours come from `web/src/theme.ts`. No new hex values in page files.
- A legend lists only states the chart beside it can draw.
- Prose says the network name capitalised (`Sepolia`), identifiers stay as the protocol writes them.
- Empty states say what would fill them and when, not just "no data".
- Nothing is announced with an exclamation mark, a metaphor, or a joke.

## Procedure

Run before committing any change under `web/`:

```bash
make e2e-fixtures
docker compose -f deploy/e2e/docker-compose.yaml up --build --force-recreate -d
cd web && npm ci && npx playwright install chromium   # once
make design                                            # WEB_BASE overrides the site under test
```

The fixture stack gives every run the same data, so a screenshot only changes when the code does.
Baselines live in `web/e2e/design/baseline/`, outside git; the first run seeds them. The check
writes the current screenshots to `web/e2e/design/current/` and a diff image for every changed
screenshot to `web/e2e/design/diff/`. Read each changed screenshot at each viewport against the
rules above. When the change is intended, run `make design-update` to accept it as the new
baseline, and say so in the commit message.

Two habits keep the check honest:

- Edit stylesheets with an exact anchor, never a file-wide substitution. `.tiles` and `.filters`
  share nothing but the text of a `minmax()` call, and a file-wide replace once widened both.
- A visual claim needs a screenshot. Do not report a layout as verified from the code alone.
