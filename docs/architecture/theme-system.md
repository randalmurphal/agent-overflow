# Theme System: Surface Survey

Status: **investigation + settled direction** (2026-08-18). Every
themeable surface mapped; §6 records the user's decisions, §7 the
reworked architecture, §8 the remaining open items.

Goal: VSCode-level user customization, meaning colors for every part of
the UI (text hierarchy, headers/bold in markdown, icons, backgrounds,
sidebar, borders), predefined popular themes for code surfaces, and a
configuration format an AI agent can be pointed at and edit sanely.

## 1. What exists today

The app already has a real semantic token system. Theming is **not** an
extraction job; it is closing ~35 leak sites and putting a user-editable
layer on top of an existing vocabulary.

### 1.1 The three token files

| File | Owns |
|---|---|
| `frontend/src/app.css` | ALL colors: the `:root` dark palette, the single `html.light` override block, and the theme-INVARIANT color roles that deliberately have no light counterpart (`--provider-claude`, `--scrim`/`--scrim-fg`), so every literal color in the app is readable in one place. Plus the `@theme` mapping (`--color-*` → Tailwind utilities like `bg-surface-1`, `text-fg-muted`) |
| `frontend/src/styles/tokens.css` | Non-color scales: type (`--text-micro..lg`), radius (`--radius-field/control/card/composer`), shadows (`--shadow-sheet/menu/modal`), plus DERIVED color roles, which are `var()`/`color-mix()` over what app.css defined and so need no light override of their own: the fg hierarchy (`--fg-muted/subtle/hint`), `--border-subtle`, `--card`, `--code-block` |
| `frontend/src/styles/syntax.css` | 21 `--syntax-*` vars (github-dark `:root`, github-light `html.light`) + the `.syntax-<name>` rules |

Derived tokens (`--fg-*`, `--border-subtle`, all three shadows, the
usage heat ramp) are `color-mix()` off base tokens. They follow a base
palette change automatically. A theme only has to set base tokens.

### 1.2 Token families (the theme vocabulary)

- **Surfaces**: `--surface-0/1/2/3`, `--card` and `--code-block`
  (aliases over surface-1), `--terminal-bg`, `--code-inline-bg`,
  `--overlay` (dims app chrome behind a modal, theme-varying)
- **On-media**: `--scrim` / `--scrim-fg`, chrome painted over user
  media, theme-invariant, consumed with opacity modifiers. Not the same
  role as `--overlay`; see the note at both definitions in `app.css`.
- **Text**: `--text-primary/secondary` + derived `--fg-muted/subtle/hint`
- **Borders**: `--border-subtle` (derived) < `--border` < `--border-strong`
- **Accent + status**: `--accent`, `--accent-fg`, `--info`, `--success`,
  `--error`, `--warning`
- **Provider identity**: `--provider-codex`, `--provider-claude`,
  `--provider-claude-tui` (all three brand-locked: identical in both
  themes, no light override)
- **Tool-kind icons**: 13 `--ico-*` keyed by the `ToolKindIcon` enum
  (`chat/toolCardHeader.ts`)
- **ANSI**: 16 `--ansi-fg-*` (chat ANSI output; "aesthetically twinned,
  not byte-identical" with the terminal, per §3.1)
- **Syntax**: 21 `--syntax-*`
- **Non-color**: shadows, radii, type scale, `--font-sans/mono`

### 1.3 Theme flip mechanics

- Setting: `settings.theme` = `system|light|dark`
  (`internal/settings/settings.go`, `allowedThemes` in `validate.go`).
- `frontend/src/lib/stores/themeMode.svelte.ts#getResolvedTheme` is
  THE single resolver (phase 1 collapsed the former duplicate): it owns
  the one `matchMedia(prefers-color-scheme)` subscription and is
  consumed by App's class-stamping effect, the xterm effects, and the
  mermaid host alike.
- `frontend/src/lib/utils/theme.ts#applyThemeClass(resolved)` is a pure
  DOM applier stamping `html.light` / `html.dark` (only `.light` is
  read by our CSS; `.dark` is kept as the conventional root marker —
  the markdown renderer's observer that read it is deleted), idempotent
  against the live DOM so no-op applies don't touch the attribute.
- Applied from `App.svelte`, alongside `applyFonts` and `applyFontScale`.
  The theme one is an `$effect.pre`: App's pre-effect stamps the class
  ahead of every descendant effect, so a consumer that reads resolved
  colors out of the DOM (the mermaid bridge's probe) never samples the
  outgoing palette on a flip. The font/zoom appliers stay plain
  `$effect`s, because nothing reads their values back.
- No FOUC guard: `index.html` has no pre-hydration script, so a
  light-theme user gets a dark first frame.

### 1.4 The runtime-override precedent: `utils/fonts.ts`

Settings → Typography fonts already do runtime token override, and the
pattern is the one to copy: `setProperty('--font-sans', …)` on `<html>`
only when the user picked a non-default; `removeProperty` at the default
so the cascade (`@theme` in app.css) stays the single source of truth.
Memoized (`lastSans/lastMono`); lazy CSS import for Hack Nerd.
`utils/zoom.ts` is the same shape for `fontSize`.

**Perf constraint for a live theme editor**: every
`setProperty` on `<html>` invalidates style for the entire document
(~13ms @ 5k nodes, ~90ms @ 30k, both measured, per the
`utils/ambientTicker.ts` header, which exists specifically to avoid root
custom-prop writes). N
tokens applied as N `setProperty` calls during a color-picker drag is a
jank machine. Apply a user theme as **one `<style id="user-theme">`
element rewrite** (single `:root {…} html.light {…}` rule block). That is
one invalidation per change, and "absent = cascade default" falls out
naturally.

## 2. Code/syntax pipeline: why code themes are nearly free

```
internal/highlight (tree-sitter, Go)
  classids.go — 29 class ids, append-only wire contract
  → EncodedLine runs of [byteLen, classId]   (IDS persisted, never colors)
  → SQLite span blobs (items.meta "codeSpans", payload span columns)
  → HighlightClassNames RPC → syntaxSpans.ts (id → "syntax-<name>", memoized)
  → class="syntax-keyword" → styles/syntax.css → var(--syntax-keyword)
```

- Persisted blobs and every cache hold **class ids, not colors**. A code
  theme swap = redefine 21 CSS vars. Zero re-highlighting, zero cache
  invalidation, zero RPC, works retroactively on all persisted history.
- The taxonomy is 29 coalesced families (not TextMate scopes): 5 ids are
  deliberately unstyled (`variable`, `parameter`, `operator`,
  `punctuation`, `embedded` inherit text color), 2 are style-only
  (`markup-bold/italic`). Importing a popular theme (Monokai, Dracula,
  …) is a **curation task**: map its scope colors onto our families.
  Coloring operators/punctuation would need new CSS rules (Go+CSS
  coordinated, append-only per `classids.go`).
- A "code theme" bundle should cover: 21 `--syntax-*`, the code-block
  background (`--surface-1` today via `.markdown-body pre` +
  `chatMarkdownTheme` `code.base`), `--code-inline-bg`, and probably the
  ANSI 16 + terminal palette so terminal-ish surfaces match.

## 3. JS/native consumers needing explicit re-notification

Exhaustive sweep result: **zero** places snapshot computed colors into
JS (no color `getComputedStyle` reads anywhere). All SVG icons use
`currentColor`; heatmap/meters/separators use `color-mix(var(…))`
inline; `ambientTicker` writes only a unitless glow scalar, and the
ambient CSS keyframes animate opacity/transform. So a
runtime palette change repaints everything for free **except**:

| Consumer | Today | For custom palettes |
|---|---|---|
| **xterm terminals** (`terminal/terminalTheme.ts`, applied in `terminalXterm.ts`, `TerminalBody.svelte`, `TakeControlTerminal.svelte`) | **BRIDGED (phase 2).** Was: hand-maintained hex `DARK`/`LIGHT` `ITheme` duplicates (44 values) that CSS vars never reached, because xterm rejects `oklch`, and the app.css comment claiming otherwise was stale. Now the `ITheme` is resolved from the live cascade through `utils/cssColorProbe.ts` (the same module the phase-1 mermaid bridge uses, so there is still exactly one resolver), and the re-apply effect tracks the palette identity rather than just the light/dark flip. See §9. | WebGL addon has a GPU glyph-atlas cache, so verify visually after palette changes. |
| **Mermaid** (`StreamdownMermaidHost.svelte` + `markdown/render/Streamdown.svelte`) | **BRIDGED (phase 1).** Was: built-in `'dark'`/`'default'` palettes only, diagram fills/strokes/labels a wholly uncaptured surface. Now `theme:'base'` + `themeVariables` resolved from app tokens (`chat/markdown/mermaidTokens.ts`, via the probe in `utils/cssColorProbe.ts`); the `{#key}` and the SVG cache key both key on the palette. | Widen the palette identity when theme files land (see §7 phase 2). |
| **Native window background** (`main_desktop.go`) | **BRIDGED (phase 2).** Was: `NewRGBA(22,22,30)` hardcoded at construction, the resize-flash color, wrong for light theme already. Now `SetWindowBackgroundColor` (LocalOnly) paints it live from the app's cascade-reading `$effect`, and construction reads the `windowBackground` cache out of `themes/appearance.json` before the webview exists. See §9.3. | — |
| **WSL launcher pages** (`cmd/agent-overflow-windows/picker.go`) | Inline HTML, hardcoded `#16161e` + Tokyo Night. Pre-app bootstrap; could at best read settings JSON off disk for light/dark. | Low priority. |
| **Favicon** (`public/favicon.svg`) | Static, dark tile. | Optional `prefers-color-scheme` inside the SVG. |
| **Second window / `--connect` client** | No `settings:changed` push event exists, so another client learns of a theme change only on reload. | Needs a push event if themes should sync live (template: `app_workflow_definitions_watcher.go`'s `a.emit`). |
| **First frame** | No FOUC guard in `index.html`. | Pre-hydration script or Wails-side class stamp. |

KaTeX: `currentColor`, nothing to do. Streamdown markdown theme
(`chat/markdown/streamdownTheme.ts`): static token-class strings, fine.

## 4. Leak inventory (~115 literals, ~35 edit sites)

> **This section is the PRE-phase-1 inventory, and it is now CLOSED**
> (landed 2026-08-18, per §7). It is kept as the historical record of
> what leaked and why each fix was chosen; line numbers and "needed"
> framing describe the tree as it was surveyed, not as it is. What keeps
> the state clean going forward is `src/lib/themeTokens.test.ts`, whose
> allowlists are shrink-only. Where the shipped design diverged from the
> proposal below, the divergence is noted inline.

Full details in the categories below; everything else swept clean
(usage, accounts, git, palette, import, discussion, panes, run-map SVGs,
primitives icons).

### 4.1 New token roles needed

1. **`--accent-fg`** (fg-on-accent): highest leverage. 4 live
   `bg-accent text-white` sites (`WorkflowActionRow.svelte:195,229,253`,
   `ReviewLineBlockRow.svelte:111`) + `settings/styles.ts:23`'s
   `text-accent-foreground` which is a **dead class** today (no
   `--color-accent-foreground` exists, so primary settings buttons render
   with ambient text color on the accent fill). A user-picked pale
   accent breaks all of these without this token.
2. **`--surface-3`**: 2 dead-class sites
   (`ProviderAccountLimits.svelte:32`, `TerminalTabStrip.svelte:146`
   use `bg-surface-3` which compiles to nothing). Define it or rewrite
   to `surface-2`.
3. **Scrim family**, for the image lightbox
   (`ExpandedImageDialog.svelte`): `bg-black/88` + `white`-based
   controls (~14 occurrences). Wants
   `--overlay-strong` + `--scrim-fg` (near-black scrim is correct in
   BOTH themes, so it must be its own role, not `--overlay`).
4. **On-media badges**: `bg-black/70 text-white` over image thumbnails
   (`UserMessage.svelte:170`, `ComposerAttachmentRow.svelte:81,87`).
   `--badge-on-media-{bg,fg}` or reuse the scrim pair.

   **Shipped differently (3 + 4 folded into one pair).** Both are the
   same statement (chrome painted over USER MEDIA), so instead of
   `--overlay-strong` plus a `--badge-on-media-*` family, phase 1 landed
   ONE theme-invariant pair, `--scrim` / `--scrim-fg`, opaque black and
   white, consumed with Tailwind opacity modifiers (`bg-scrim/88`,
   `text-scrim-fg/78`, `ring-scrim-fg/70`). Storing them undiluted is
   what lets one pair serve every dilution the lightbox and the badges
   each wanted. They are deliberately distinct from `--overlay`, which
   dims APP CHROME behind a modal and therefore does vary by theme.
   The vocabulary note lives at both definitions in `app.css`.
5. **`--provider-claude`** retires `text-[#d97757]`
   (`ProviderIcon.svelte:28`), completes the provider family. Brand
   colors stay default-locked but become visible to a theme editor.
6. *(optional)* `--shadow-accent-inset` (dup'd arbitrary shadow in
   `ComposerPendingApprovalPanel.svelte:36` /
   `ComposerPendingUserInputPanel.svelte:345`, already token-derived,
   just unnamed).

### 4.2 Straight swaps to existing tokens

- `bg-black/45` scrims → `bg-overlay`: `primitives/Modal.svelte:134`,
  `primitives/OverlayShell.svelte:40` (`DiagramModal.svelte:242` already
  does it right). Note `Modal.test.ts:232` pins the literal.
- `text-red-400` ×2 → `text-error`: `settings/ArchivedThreads.svelte:104,136`.
- Default Tailwind shadows → token shadows (10 sites, of which the
  10th,
  `shared/ToggleSwitch.svelte:29`, was missed by this survey and caught
  by the phase-1 tripwire test): `shadow-sm` →
  `shadow-sheet` in `MessageTimeline.svelte:969`,
  `ProposedPlanCard.svelte:91`, `ComposerAttachmentRow.svelte:56`,
  `McpServersMenu.svelte:228`, `ThreadRow.svelte:466`,
  `WorkflowRunMap.svelte:295`; `shadow-lg` → `shadow-menu` in
  `LazyOverlay.svelte:44` and `ThreadRow.svelte:276` (JS-built drag
  ghost `className`, invisible to class scanners);
  `shadow-[0_1px_4px_rgba(0,0,0,0.25)]` → `shadow-sheet` in
  `ReviewDiffBody.svelte:390` (the only raw `rgba()` in src);
  `shadow-[var(--shadow-sheet)]` → `shadow-sheet` in `Segmented.svelte:52`.

**Three of these were NOT value-preserving, and shipped as accepted
visual changes** (the rest are exact swaps):

1. **Modal / OverlayShell backdrops**: `black/45` → `--overlay`, i.e.
   60% black in dark mode and a 28% light wash in light mode. The
   backdrop got denser in dark mode and changed character in light mode,
   deliberately, because it unifies the two shells with
   `DiagramModal`, which already used `bg-overlay`. Three modal
   surfaces disagreeing on backdrop density was the bug.
2. **`ReviewDiffBody` sticky-header shadow**: `0 1px 4px /25%` →
   `--shadow-sheet` (`0 1px 2px /20%`). Slightly tighter and lighter.
   Accepted: the point of the shadow scale is that one bespoke value
   does not get to sit half a step off the role it belongs to.
3. **Settings primary buttons**: `settings/styles.ts`'s
   `text-accent-foreground` was a DEAD class (no such token existed), so
   the label rendered in ambient text color on the accent fill. Pointing
   it at `text-accent-fg` gives it the white label it always intended:
   a visible change, and a fix rather than a regression.

### 4.3 Streamdown keys that reached the screen unthemed — CLOSED

Historic: `chatMarkdownTheme` (`streamdownTheme.ts`) used to be an
override layer merged over a vendor base theme, and 9 colored keys leaked
that base's palette classes: `alert.note/tip/warning/caution/important`
(GFM `> [!NOTE]` etc. → `text-blue-600`-family), `del.base` (`~~strike~~`
→ `text-gray-600`, likely in real agent prose), `footnoteRef.base`,
`descriptionTerm/Detail.base`. All 9 now carry token entries
(`--info/success/warning/error/accent` + `--fg-subtle/hint` +
`--card`/`--border-subtle`), no new tokens; the merge and the base theme
are both gone — `chatMarkdownTheme` is the whole table. (`code.header/
skeleton/…` and `inlineCitation.*` were verified unreachable and deleted.
`markdown/render/elements/Image.svelte`'s blocked-image chip, unreachable
with `ALLOWED_IMAGE_PREFIXES=['*']`, was tokenized when that tree entered
`src/` and Tailwind's scan.)

### 4.4 Deliberate literals: keep, and mark as such

`imageCompress.ts:110` (white matte on exported artifacts; the
`MermaidDownload.svelte` that carried the second one was deleted with the
renderer's download chrome), `UserMessageBody.svelte:142` (mask alpha
channel, not a color), design-panel `bg-white` iframe paper, the brand
coral **value** (tokenize the name, lock the default).

### 4.5 Tests pinning colors

`terminalTheme.test.ts` (exact hex), `ProviderIcon.test.ts` /
`ModelProviderMenu.test.ts` (`text-[#d97757]`), `Modal.test.ts`
(`bg-black/45`), `internal/settings/validate_test.go` (asserts theme
`"solarized"` is rejected, so any new theme id must extend
`allowedThemes` or the whole selection model changes),
`workflowRunMapStyle.test.ts` (exactly-one `--success` use: safe under
recoloring, breaks under token renames).

## 5. Settings/persistence facts that shape the design

- Settings = `<configDir>/settings.json`, **sparse-written** (only
  non-default keys), atomic rename, mtime-based lazy reload in
  `Service.Get()`. An external hand-edit IS picked up backend-side
  without restart. Frontend gets settings only via RPC return values;
  **no push event**.
- Sparse-write is hostile to agent discoverability: an agent pointed at
  `settings.json` sees a nearly-empty file. A theme file should be
  fully materialized (or ship a schema/reference doc beside it).
- **`keybindings.json` is the strongest precedent for a separate
  file**: own package + `Service` (`Get/Update/Reset`), own atomic
  write, defaults + user-override merge, read RPC deliberately
  LAN-allowed while writes stay in `LocalOnlyMethods`. A theme read is
  the same class as `GetKeybindings`; theme writes are local-only.
- The in-flight prompt-overrides work establishes the in-`settings.json`
  alternative: typed sub-struct in its own file, `omitempty`, bounds
  consts, validate/sanitize dual, accessor methods. Collision risk with
  it is textual only (`sections.ts`, `SettingsView.svelte`,
  `settings.go`, `validate.go` are all in its working tree).
- No watcher exists on config files; the template for one is
  `app_workflow_definitions_watcher.go` (250ms debounce, and its
  root-watch filter already deliberately ignores settings.json's atomic
  renames, so a theme watcher must not feed back on its own writes).
- Adding a flat setting costs 3 sync points today (Go struct, Go
  defaults, TS interface — the TS defaults are generated) + allow-list +
  section UI. Another argument for one `theme` reference + separate theme files
  over N flat color keys.
- Scope: every appearance setting today is app-global. The per-client
  `ui_state` precedent exists (pane layout), but mixing would be novel.

## 6. Decisions (user, 2026-08-18)

1. **Two axes**: UI theme × code theme, independently selectable.
2. **Separate theme files**, not settings.json keys.
3. **Per-client scope.** Stated explicitly: a client will (future) be
   able to connect to multiple backends at the same time, and different
   clients may carry different theme configs. Theme is a property of
   the client machine, NOT of a backend, so it must not live in
   backend `settings.json` and must not live in the backend-side
   `ui_state` table either (per-client-per-backend would duplicate and
   drift across backends).
4. **Local-only writes, per client.**
5. **Everything sensible is in the schema**, provider colors included.
   Deliberate exceptions (export mattes, mask alpha, design-canvas
   paper defaults) stay, but as visible schema entries or documented
   exclusions, never invisible literals.
6. **VSCode's shape** (named theme files, base + sparse overrides,
   semantic vocabulary), not TextMate scopes.

### 6.1 Client-residency reality check (verified in code)

- Desktop mode: backend + client are ONE process; `<configDir>` is both
  the backend's data dir and the client machine's config home. A
  client-side `themes/` dir works with all existing infra (fs watcher
  template, App-bound RPCs, `a.emit`).
- `--connect` client mode (`main_desktop.go#runClient`): the client
  process registers **no services**. The SPA RPCs entirely against the
  remote backend; the local process is a static stub
  (`internal/clientmode`) that injects `window.__AO_BOOTSTRAP__` into
  index.html. But the client binary DOES have a durable per-machine
  ClientID (`ensureClientID`) and its own `<configDir>` on the client
  machine. So client-side theme files are *storable* there today, but
  there is no live channel from the stub to the webview, so theme data
  would ride the bootstrap injection (applied at page load; live
  file-watch reload needs a small stub endpoint later).
- Pure-browser remote sessions: no local process, no files. Built-in
  themes + localStorage selection only.

This staging is the price of decision 3 and is acceptable: the primary
surface (desktop app) gets the full feature; `--connect` gets themes at
load; browsers get built-ins.

### 6.2 Consequences for existing settings

`settings.theme` (and arguably `sansFont`/`monoFont`/`fontSize`, the
same per-client appearance class; the projector case) migrates out of
backend settings into the client-side appearance config. Backend fields
retire via `retiredSettingsFieldNames()`, with a one-time read of the
old value into the client file at first boot. Recommended scoping: move
`theme` in phase 2; leave fonts/fontSize backend-global initially and
migrate in a follow-up (they have working plumbing and the split, while
philosophically inconsistent, costs nothing until multi-backend lands).

## 7. Architecture (per decisions)

### Phase 1: close the leaks (decision-free, can start now)

Add the §4.1 tokens, do the §4.2 swaps, add the §4.3 streamdown keys,
bridge mermaid `themeVariables`, fix the two dead-class families,
collapse the duplicate `matchMedia` resolvers, fix the stale app.css
comment about terminalTheme. Update the pinned tests. End state:
**100% of app chrome resolves through the token vocabulary**, plus a
tripwire test that greps for raw palette classes / literals so the
state stays clean.

**Landed 2026-08-18.** What shipped: the §4.1 tokens (`--accent-fg`,
`--surface-3`, the `--scrim`/`--scrim-fg` pair that folded items 3+4,
`--provider-claude`, `--shadow-accent-inset`, `--code-block`); the §4.2 swaps,
three of them accepted visual changes
as noted there; the nine §4.3 streamdown keys; the mermaid
`themeVariables` bridge (`chat/markdown/mermaidTokens.ts` +
`utils/cssColorProbe.ts`, palette-keyed remount and SVG cache); the
resolver collapse to one `getResolvedTheme` owning the single
`matchMedia` subscription, with App stamping the class from an
`$effect.pre`; the stale app.css comment about `terminalTheme` fixed;
and the tripwire, `src/lib/themeTokens.test.ts`, whose raw-class
allowlist is EMPTY. That emptiness is the phase-1 completion claim.
Still open from this phase's wish list: the FOUC guard and the native
window background, both carried into phase 2 below.

### Phase 2: theme files, selection, live reload (desktop)

- **Files**: `<configDir>/themes/*.json` on the client machine. Each
  file: `{ "$schema": …, "name", "extends": "dark"|"light",
  "colors": {surface,text,border,accent,status,provider,icons,…},
  "syntax": {…}, "ansi": {…}, "terminal": {…} }`, sparse overrides
  over the extended base. Agent discoverability comes from the JSON
  schema + a generated `themes/TOKENS.md` reference listing every
  token, its role, and its default per base, NOT from materializing
  every value into every file (materialized files go stale the moment
  the app grows a token; `extends` inherits new tokens automatically).
- **Selection** (user-confirmed model, 2026-08-18): client-side
  appearance config (same dir), `{ mode: system|light|dark,
  uiTheme: id, codeTheme: id }`. ONE theme per axis; a theme FILE may
  define a `light` palette, a `dark` palette, or both (the built-in
  default defines both, exactly today's `:root` + `html.light` pair),
  and `mode` picks which palette renders. Missing-variant rules differ
  by axis, deliberately: a dark-only UI theme in light mode falls back
  to the default light palette (chrome must match the mode); a
  dark-only CODE theme stays itself in light mode, self-contained:
  block-code surfaces own their backgrounds (`--code-block`,
  `--terminal-bg`), so a Monokai block on a light
  UI renders as a dark island, the familiar docs-site pattern, instead
  of unreadable dark-on-light text. (Inline code chips are UI-axis
  prose chrome, per §9.12.) The `html.light` class flip stays
  pure CSS.
- **Application**: resolve both axes into ONE
  `<style id="user-theme">` rewrite containing `:root {…}` +
  `html.light {…}` override blocks (single style invalidation, per the
  §1.4 perf note; absent token = cascade default, the fonts.ts
  principle).
  A single resolved-appearance store carries palette identity; the
  xterm effects and mermaid `{#key}`/cache key track it.
- **Terminal hex bridge**: resolve token values to hex/rgb for xterm,
  replacing the hand-maintained `DARK`/`LIGHT` duplicates in
  `terminalTheme.ts`. **Reuse `utils/cssColorProbe.ts`**, the module
  phase 1's mermaid bridge already does this through; do not write a
  second resolver. Its mechanism is not the obvious one, which is why
  the module exists: `getComputedStyle` does NOT normalize colors to
  `rgb()`. Computed styles serialize in their DECLARED color space, so
  an `oklch()` token reads back as the `oklch()`/`oklab()` string, which
  xterm rejects. Resolution therefore needs the probe element AND a
  1×1-canvas readback (paint the color, read the pixel) to land on real
  channel values. `mermaidTokens.browser.test.ts` pins that behavior in
  a real browser. A jsdom test cannot see it, which is precisely how
  the "browser normalizes to rgb" assumption survived this long.
- **Live reload**: fsnotify watcher on `themes/` + the appearance file
  (template: `app_workflow_definitions_watcher.go`; must ignore its own
  atomic renames) → `theme:changed` emit → frontend re-resolves. This
  is the agent-edit loop. Errors are user-facing: a broken theme file
  surfaces a visible warning and falls back per-token, never silently.
- **Native window background** binding (classified in
  `LocalOnlyMethods`) + FOUC stamp for the first frame.
- **Transport posture**: theme reads LAN-allowed (keybindings parity),
  writes local-only; view-only sessions render with built-ins or the
  values the bootstrap/read path supplies.

### Phase 2/3 concrete contract (build spec, 2026-08-18)

Fixed here so backend, frontend-core, and integration work can proceed
in parallel against one text.

**Files** (all under `<configDir>/themes/` so one watcher and one
`ls` covers everything an agent needs):

- `themes/*.json` are the theme files. `id` = filename stem
  (kebab-case).
- `themes/appearance.json` holds the selection:
  `{ "mode": "system"|"light"|"dark", "uiTheme": id, "codeTheme": id,
  "windowBackground": "#rrggbb"? }`. `windowBackground` is a CACHE the
  frontend maintains (last resolved `--surface-0`-family value) so the
  native window can be created with the right color before the webview
  paints; it is never user-edited semantics.
- `themes/theme.schema.json` + `themes/TOKENS.md` are the generated
  reference, seeded/refreshed by the backend at boot from Go-embedded
  assets (`internal/theme/assets/`), which are in turn generated from
  the frontend token registry by
  `frontend/scripts/generate-theme-reference.mjs` and committed; a test
  fails when registry and committed assets drift.

**Theme file format** (VSCode shape, per-mode variants):

```json
{
  "$schema": "./theme.schema.json",
  "name": "Display Name",
  "dark":  { "colors": {…}, "syntax": {…}, "ansi": {…}, "code": {…} },
  "light": { … }
}
```

- A variant block is SPARSE over the built-in base of that polarity.
- Section key spaces come from the frontend token registry
  (`frontend/src/lib/theme/tokenRegistry.ts`), one-to-one with CSS var
  names minus the `--` (e.g. `"surface-1"`, `"accent"`,
  `"ico-terminal"`, `"syntax-keyword"`, `"ansi-fg-31"`). `colors` is
  the UI axis; `syntax` + `ansi` + `code` are the code axis (`code`
  holds the block-code grounds: `code-block`, `terminal-bg`; the
  inline-code chip pair is UI-axis, per §9.12).
- A file that defines `colors` in any variant is listable on the UI
  axis; one that defines any code section is listable on the code
  axis; a file may serve both.
- Built-in ids are reserved: `default` (UI, both variants: the
  app.css palette, an identity theme emitting no CSS) and `github`
  (code, both variants: syntax.css, also identity). A user file with
  a built-in id shadows it and is listed as user-sourced.
- Variant pick: UI axis uses the variant matching resolved mode, else
  falls back to built-in default entirely for that mode; code axis
  uses the matching variant, else its sole variant (dark island).

**Backend** (`internal/theme/`, keybindings-service parity;
Go is pipe and never parses theme JSON beyond `appearance.json`):

- `GetThemeFiles() → { dir, themes: [{id, raw}], appearance,
  warnings: [string] }` is one RPC, LAN-read-allowed (keybindings
  parity). Unreadable files/dir problems land in `warnings`.
- `SetAppearance(appearance)` is LOCAL-ONLY (`LocalOnlyMethods`),
  atomic write, validated (mode enum, id shape, hex shape), sparse.
- Watcher on `themes/` (template
  `app_workflow_definitions_watcher.go`: 250ms debounce, ignore own
  atomic renames) → `a.emit("theme:changed")`; frontend refetches.
- Boot: ensure dir, seed/refresh schema + TOKENS.md, and if
  `appearance.json` is absent seed `mode` from legacy
  `settings.theme`. The settings field retires
  (`retiredSettingsFieldNames`) in the same change set that moves the
  frontend picker off it.
- Native window: `BackgroundColour` at window creation reads
  `appearance.windowBackground` (fallback: current hardcoded value);
  a `SetWindowBackgroundColor` App method (LOCAL-ONLY) applies live
  changes.

**Frontend** (`frontend/src/lib/theme/`):

- `tokenRegistry.ts` is THE canonical token list: section, JSON key,
  CSS var, axis, description. A test parses app.css/tokens.css/
  syntax.css and fails on drift in either direction.
- Pure core: parse/validate (structural in pure code; value
  validation via `CSS.supports('color', v)` at apply time, where invalid
  values are SKIPPED per-token with a visible warning, never fatal),
  resolve (appearance + themes + builtins → one CSS text of
  `:root {…}` + `html.light {…}` blocks + palette identity string +
  warnings). `--accent-fg` auto-derives by contrast (via
  `utils/cssColorProbe.ts` luminance readback) when a theme sets
  `accent` but not `accent-fg`.
- Application: ONE `<style id="user-theme">` element rewritten
  wholesale (§1.4 perf note); absent token = cascade default.
- Palette identity: `mermaidPaletteIdentity()` and the xterm bridge
  widen with `uiTheme|codeTheme|revision`; xterm's hand-maintained
  `DARK`/`LIGHT` duplicates in `terminalTheme.ts` are replaced by
  probe-resolved values (`cssColorProbe.ts`).
- FOUC: last-applied mode class + window background stamped in
  `localStorage`, read by an inline script before first paint.
- Remote/browser sessions (`--connect`, LAN browser): built-in themes
  + `localStorage` selection; the theme RPCs target the DESKTOP's own
  config dir and are refused remotely per their classification, so
  the store must degrade cleanly when they are unavailable.

### Phase 3: code-theme bundles + remote clients

- Curated popular palettes (GitHub dark/light [current defaults],
  Monokai, Dracula, Solarized, Tokyo Night, Catppuccin, …) shipped as
  built-in code themes: syntax 21 + ansi 16 + terminal + code-block/
  inline backgrounds, mapped by hand onto the 29-family taxonomy.
- `--connect`: theme payload injected via `__AO_BOOTSTRAP__`; optional
  stub endpoint for live reload. Browser sessions: built-ins +
  localStorage selection.
- Optional later: in-app theme editing UI. The file format is the
  contract either way; agents are the primary editor.

## 8. Resolved design stances + residual items

All originally-open questions are settled (user 2026-08-18, with a
"use best judgement — easy to customize, hard to fk up" mandate for
mechanism details). Stances taken under that mandate:

1. **Selection shape**: confirmed as §7 phase 2. One theme per axis,
   themes carry per-mode palettes, mode picks the variant.
2. **Fonts/fontSize**: feature stays as-is; only its STORAGE moves to
   the client-side appearance config in a follow-up (per-client
   consistency), zero behavior change.
3. **Hard-to-fk-up posture**: `extends` + sparse overrides (you can
   only break what you touched); per-token validation with visible
   warning + per-token fallback (one bad value never kills a theme);
   `--accent-fg` auto-derived by contrast when unset; derived tokens
   (`--fg-*`, `--border-subtle`, shadows) keep following base tokens
   unless explicitly overridden; schema + generated TOKENS.md so
   agents/humans discover rather than guess.
4. **Code-theme boundary**: new `--code-block` (default
   `var(--surface-1)`) so the code axis owns block backgrounds without
   dragging the general elevation tier.
5. **Multi-backend client** doesn't exist yet; this design just avoids
   blocking it (nothing theme-shaped is stored backend-side except the
   retired-field migration read).

## 9. As built (integration wave, 2026-08-18)

The contract above is what was built. This section records where the
implementation is MORE specific than the contract, and the four places
it deliberately diverges. Everything here is load-bearing: each item
exists because the obvious reading of the contract produced a visible
bug.

### 9.1 Ordering: both appliers are render effects

`App.svelte` runs the mode-class stamp and the style rewrite as two
`$effect.pre` bodies in declaration order, then the cascade-reading work
as a plain `$effect`:

```
$effect.pre  → applyThemeClass(getResolvedTheme())
$effect.pre  → applyTheme({ mode, appearance, themes, revision })
$effect      → readWindowGroundHex() → stampBootTheme() → syncWindowBackground()
```

Svelte flushes ALL render effects for the whole tree before ANY user
effect. A class stamp in the render pass paired with a style rewrite in
the user pass therefore leaves the ENTIRE render pass with the document
saying `light` while `<style id="user-theme">` still holds the dark
resolution. The resolved MODE is a resolver INPUT (a UI theme with
only a dark variant is emitted in dark mode and not in light), so the
mismatch is real CSS, not a cosmetic window. Both halves in the render
pass close it for pre-effect and user-effect readers alike.

The third block is a user effect for the opposite reason: it READS the
cascade back and WRITES store state, neither of which belongs in the
render pass. It cannot loop: `syncWindowBackground` moves only
`windowBackground`, which re-runs the second pre-effect to an identical
`AppliedTheme`, which `sameApplied()` refuses to reassign, so the user
effect's own dependency never changes.

### 9.2 The style element lives in `<body>`

Vite appends the app's stylesheet `<link>`s to the END of `<head>`, so a
`<style>` the boot script inserts into the head loses the source-order
tie to `app.css`'s `:root` and the cached palette is ignored at exactly
the moment it matters. `index.html` therefore declares
`<style id="user-theme"></style>` in the body, before `#app`, and the
boot script sits beside it. The applier rewrites that same element by
id, so there is still exactly one user-theme style in the document,
and no `!important` anywhere. Pinned by `theme/themeBootStamp.test.ts`,
which greps `index.html` for the storage key, the element id, the size
cap and the hex guard, because the boot script cannot import a constant.

### 9.3 The window ground is PROBED, not read from the resolver

`ResolvedTheme.windowBackground` reports only what a theme FILE
contributed. The built-in palette also sets the ground, and states it in
`oklch`, which is neither a hex the native window can take nor something
the resolver reports. `readWindowGroundHex()` therefore probes the live
`--surface-0` through `utils/cssColorProbe.ts` and normalizes to
`#rrggbb`, which answers for both cases with what is actually on screen.
It must run after the class stamp and after `applyTheme`, which is why
it is in the user effect.

### 9.4 Terminal selection is accent-derived, not a token

The old xterm palette stated `selectionBackground` as an opaque slab plus
a matching `selectionForeground`, which meant restating the text color
per theme. The bridge now tints `--accent` at alpha 0.4 (0.22 inactive)
and emits no `selectionForeground`, so glyphs keep their own colors
underneath and a code-colored line stays readable while selected. This
is a deliberate visual change, and it is why no `--selection` token was
added to the registry.

### 9.5 `settings.theme` retirement

The field is gone from the Go struct, from `DefaultSettings`, from
`allowedThemes`, and from the TS `Settings` interface; `"theme"` is in
`retiredSettingsFieldNames()`. `initThemeDirectory` still seeds
`appearance.json`'s mode from it on first boot, through
`Service.RetiredString("theme")`, a raw `map[string]json.RawMessage`
read of the file, because a retired key is by definition not on the
struct. Retired keys are DROPPED by the next sparse write, and that is
safe here only because of boot-path ordering: `initThemeDirectory`
consumes the value before any `Update` can reach the file.
`TestInitThemeDirectorySeedsModeFromRetiredSettingsField` pins both
halves.

### 9.6 Degradation

`stores/appearance.svelte.ts` is a plain module store in the shape of
`settings.svelte.ts`, not an `entityStore` and not a
`keyedSignalRegistry`: appearance is one app-global value, and the only
backend resource is a watcher the BACKEND owns for the process lifetime,
so there is nothing to acquire and nothing to release.

Degradation is THREE facts, not one flag (revised in the review round,
§9.9, because the single flag conflated a structural refusal with a failed
read):

- **`readAvailable`**: `GetThemeFiles` answering `method_not_found`.
  Not an error to report: it is a session without a themes directory.
  Built-ins only, and the settings surface says so.
- **`writesRefused`**: a refused `SetAppearance` or a refused read, and
  a view-only session is write-blocked up front rather than after a
  failure. A write-blocked session still TAKES the wire's themes,
  directory and warnings; what it never adopts is the SELECTION. That is
  the client-residency decision (§6.1) enforced at the read: a remote
  browser renders its own choice out of `localStorage`, which is the only
  copy such a session can have, and is not repainted by whoever is at the
  desktop.
- **`loadError`**: a transient read failure. It surfaces, keeps writes
  enabled, and keeps the themes already loaded. Nothing latches.

localStorage is also the first-paint cache and is written on every
selection change even when the RPCs ARE available, because the boot
script reads it before the bundle loads.

### 9.7 Where the RPCs live

All three theme RPCs are called from the store, never from
`lib/theme/`, and are registered in `architecture.test.ts`'s
`ENTITY_OWNED_BINDINGS` against it. `lib/theme/` stays RPC-free and
therefore testable as pure code plus one browser suite.

### 9.8 No open-folder affordance

Settings → Theme names the themes directory as selectable text and
stops there. `OpenInEditor` is the only open-a-path binding, and
`editor.ResolvePath` refuses DIRECTORIES anywhere by design (the
markdown-link work made that a security boundary, not an oversight), so
there is no existing pattern to reuse and one was not invented. The path
is copyable, which is what an agent-driven editing loop actually needs.

### 9.9 Review round (2026-08-18)

Six-lens review of the integration wave. Behavior that CHANGED, in one
line each:

- **A remote session keeps its own selection.** §9.6's one degrade flag
  split into three independent facts: `readAvailable` (no themes
  directory at all), `writesRefused` (structural, and view-only sessions
  are write-blocked up front), and `loadError` (transient, latches
  nothing). A write-blocked session still takes the wire's themes,
  directory and warnings but never adopts its SELECTION, which stays in
  `localStorage`: the theme is a property of the CLIENT, so a browser
  attached to someone's backend must not be repainted by whoever is at
  the desktop.
- **The value gate rejects the `url()`/`var()` families.** A token value
  that resolves through another declaration or fetches is not a color,
  whatever `CSS.supports` says about it.
- **Revision bumps are content-gated.** A refetch that returns identical
  bytes no longer bumps the palette identity, so it no longer remounts
  every mermaid diagram and rebuilds the xterm atlas.
- **`theme:changed` is latest-only on reconnect**
  (`internal/transport/event_visibility.go`). The frames are payload-less
  refetch signals, so a ring's worth of them replayed as N identical
  full-listing refetches; a capacity-1 ring delivers the one that
  matters. `workflow:definitions-changed` had the identical gap and is
  classified alongside it.
- **The `settings.theme` migration survives a failed boot.** The retired
  key is dropped by the next settings write whether or not the seed
  succeeded, so a boot that could not create `themes/` now carries the
  pending mode in process state and re-seeds from the next
  `GetThemeFiles`. §6.2's one-shot read had exactly one un-handled
  failure mode, and this is it.

### 9.10 Markdown prose roles + theme character round (2026-08-19)

User verdict on the shipped UI themes: "a colored film over everything —
most themes look basically the same." Root causes, confirmed by
side-by-side harness screenshots: dark editor grounds are near-identical
grays, so a ground swap reads as a tint; some curated palettes reused
the upstream's muted EDITOR foreground as chrome text (one-dark's
#abb2bf focal → body copy at 80% of that, i.e. hazy); and chat prose,
the surface a reader actually looks at, had no themeable text roles at
all, so nothing characterful could change.

What changed:

- **Six `md-*` tokens** (`md-heading`, `md-bold`, `md-link`,
  `md-inline-code`, `md-blockquote`, `md-marker`), derived, declared in
  `tokens.css` and consumed by the `.markdown-body` rules in app.css.
  Five are UI-axis `colors` tokens; `md-inline-code` first landed as a
  CODE-axis `code` token because its GROUND was one
  (`--code-inline-bg`). A UI-axis text color over a code-axis ground
  could pair unreadably under mixed axes, so the role lives beside its
  ground and one theme supplies both (post-task-review finding, three
  lenses). §9.12 later moved the PAIR to the UI axis; the
  same-theme-supplies-both invariant is unchanged. Defaults
  derive from the text palette (heading/bold/inline code get a small
  focal lift to `--text-primary` over the fg-muted body tier; link
  follows `--accent`; quote and marker keep their prior tiers), so the
  default theme keeps its quiet look. Registry 79 → 85 tokens
  (44 colors / 4 code); schema/TOKENS.md regenerated. Precedence
  carve-outs in app.css (pinned by `markdownCss.test.js`): bold
  inherits inside headings, links and quotes; inline code inherits
  inside links (live and blocked), so a clickable path label reads as
  a link; `pre code` stays on the block's own text; code chips inside
  headings/quotes deliberately keep the chip color. There is no
  utility form for these tokens. The `--color-md-*` aliases exist so
  `streamdownTheme.ts`'s class table can name the same token the
  cascade paints, and so cannot disagree with it.
- **Curated themes state the prose roles** from the same hues their
  code axis gives the `markup-*` families (so chat prose and fenced
  markdown agree when both axes are on the theme), plus a per-theme
  bold pick. Bold has no `markup-*` counterpart, so each theme picks
  one emphasis hue, usually its warm accent. Adoption is pinned
  (builtins.test.ts): all 13 UI-axis curated variants state the five
  prose roles and the chip text. A variant whose palette has no
  floor-clearing hue for a role restates its own text tier instead
  (latte and solarized-light bold; latte, solarized-light and
  gruvbox-light chip text). Weight and the chip ground carry the
  role, and the statement stays explicit because of the
  shared-default rule below.
- **Contrast floors for stated md-* values**
  (builtins.contrast.test.ts): headings 3:1, a deliberate
  palette-fidelity concession for the heaviest, most isolated text
  (NOT a WCAG large-text claim; chat headings are 15-18px), which is
  what keeps solarized's canonical orange at 3.3:1; bold/link 4:1
  (body-size but carrying a second cue); quote/marker 3:1;
  `md-inline-code` measured against the SAME variant's
  `code-inline-bg`, not surface-0, the ground it actually paints on
  (this floor is what caught gruvbox-light aqua at 3.64:1 and
  high-contrast light green at 6.45:1, both re-picked). High Contrast
  is held to 7:1 on all of them.
- **one-dark chrome text brightened**: focal #abb2bf → #d7dae0 (the
  theme's own bright tier), secondary → #abb2bf. Fixes the washed-out
  chrome; body copy at the fg-muted tier now lands on the classic
  editor gray.
- **User bubble carries the accent** (`UserMessage.svelte`:
  `bg-accent/15` + `border-accent/20`, was neutral surface-2/60), the
  one structural accent surface in the transcript, which is what makes
  each theme's identity legible at a glance. `/15` matches the
  sidebar's selected-row tint, the precedent for accent presence.

The review round's structural find is **the shared-default (mode-leak)
rule**. The emission model is "dark variant → `:root`, light variant →
`html.light`", and every token whose app default is declared ONCE for
both modes (the `modeInvariant` literals, plus ALL tokens.css-declared
derived roles, since that file's light block is pinned empty) has
nothing in `html.light` to out-cascade a stated `:root` value. A
two-variant theme stating such a token in its dark variant alone
therefore paints the DARK literal in light mode: found live when latte
omitted `md-bold` and rendered mocha's mustard on `#eff1f5` at 1.12:1.
Derivedness protects only the DEFAULT. A stated literal replaces the
`var()` that was carrying the mode. Enforcement is three-layered, all
reading ONE predicate (`isSharedDefaultToken` in tokenRegistry.ts):
the resolver's `mode-invariant` warning now fires for derived roles
stated in one variant of a pair (user files get told); a curated theme
is TESTED to state a shared-default token in both variants or neither
(builtins.test.ts); and `tokenRegistry.test.ts` derives the predicate's
correctness from the stylesheet parse itself, so a future `:root`-only
derived token in app.css fails the suite instead of stranding silently.
Same round: the boot-CSS validator in index.html now refuses the
serializer's `REFUSED_FUNCTIONS` (`url(` and kin) inside its line
grammar. The cached-CSS path paints pre-CSP on a `background`
shorthand, so a hostile localStorage stamp was a first-frame network
beacon (pre-existing; hostile cases added to themeBootStamp.test.ts).

### 9.11 Luminance-range rule: the haze fix (2026-08-19)

User verdict on the character round: every curated theme except default
and high-contrast still read as if under "a hazy layer... not crisp and
clear". Root cause, measured: the default dark palette runs a ~17:1
luminance range (ground `oklch(0.145 …)` ≈ #0b0b10 under ~#f0f0f2 focal
text, 11:1 body), while every curated dark variant anchored its surface
ladder at the upstream's CANONICAL editor background (a tone those
themes design as the top of a deeper ramp), compressing the app into
10.6–13.4:1 focal / 7.2–7.8:1 body (solarized: 5.6/4.1). The sub-focal
text stack amplifies it: `--fg-muted/subtle/hint` derive by
alpha-fading `--text-primary` (80/55/30%), so a compressed palette pays
the fade twice. High-contrast was immune because it overrides the
derived tiers opaquely on true black; that was the tell.

**The rule: a curated dark ladder ENTERS at the bottom of the
upstream's published range (or one documented continuation below it),
not at the canonical editor ground.** The canonical background becomes
the card tier, where content actually sits, and the range widens
without inventing hues:

- **one-dark** `#16191d` (continuation) → `#1e2227` → `#2c313a` → `#404754`;
  focal text takes the palette's ANSI bright-white `#e6e6e6` (both dimmer
  candidates, editor fg `#abb2bf` and ANSI 37 `#d7dae0`, left the app
  gray beside the default's near-white, and the fade tiers pay focal grayness
  twice)
- **dracula** bgDarker `#191a21` → bgDark `#21222c` → bg `#282a36` → bgLight `#343746`
- **catppuccin mocha** mantle `#181825` → base `#1e1e2e` → surface0 → surface1
- **gruvbox** hard-contrast pairing both modes: `bg0_hard #1d2021` + `fg0 #fbf1c7`
  (light: `#f9f5d7` + `#282828`)
- **nord** darkened-nord0 `#242933` (the ports' tone, same standing as
  `#616e88`) → nord0 → nord1 → nord2
- **tokyo-night** ground already bg_dark; the fix is the LADDER TOP
  (bg_highlight `#292e42`, fg_gutter `#3b4261`, borders up a step) plus
  the text side: `fg-muted` stated = fg `#c0caf5` (upstream renders body
  at fg; the derived 80% tier landed at 7.2:1 with no brighter published
  tone to lift focal to)
- **solarized dark** `#00212b` continuation → base03 → base02 → `#0e4653`;
  focal text takes base3 `#fdf6e3` (the palette's whitest tone, the one
  Solarized terminals map BRIGHT white onto, as our ANSI 97 already did),
  with `fg-muted` stated = base1; light mirrors it (focal base02,
  `fg-muted` base01)

Code grounds (`code-block`/`terminal-bg`) follow each new ladder floor.
Resulting bands: focal 11.1–16.3:1 (one-dark 14.1, solarized 15.5 after the whiter-focal follow-up), body 6.3–11.1:1. The curated set
now brackets the default instead of trailing it.

Stating a fade tier is a CLAIM: `builtins.contrast.test.ts` holds any
stated `fg-muted/subtle/hint` to 4.5:1 (that is why the theme
bothered), so themes state only `fg-muted` and let subtle/hint keep
deriving, because de-emphasis tiers recede by design. Side effect of
the darker solarized code ground: canonical base01 comments now measure
3.12:1 and the long-standing `solarized.dark.syntax-comment` exception
is DELETED (the test demands deletion once an exception clears the
floor).

### 9.12 Inline-code chip pair moved to the UI axis (2026-08-19)

`code-inline-bg` + `md-inline-code` moved from the `code` section
(code axis) to `colors` (UI axis). User-driven, and the user was
right: the inline chip is monochrome prose furniture (a sibling of
bold and links, with no syntax in it), so "the code theme owns code"
never actually applied to it; that rationale is about highlighting and
the dark-island behavior of BLOCK surfaces. The §9.10 invariant was
narrower than where it first landed: the chip text and chip ground
must come from the SAME theme so no UI/code combination can split the
pair, and that is satisfied equally well with both tokens on the UI
axis, which is also the convention (docs sites keep inline chips prose-matched
even when code blocks are dark islands).

Consequences: registry 46 colors / 2 code (`code` is now block
grounds only: `code-block`, `terminal-bg`); every UI-axis curated
variant states the pair in `colors` (adoption + chip-floor tests
follow it there); monokai, deliberately code-only, no longer states
`code-inline-bg`, so inline chips under a monokai code axis follow the
UI theme like every other prose role. Visible behavior change: with a
light UI over a dark-only code theme, inline chips are now
prose-matched instead of tiny dark islands. What surfaced it: a user
theme stated `md-inline-code` and the edit silently changed nothing,
because the selected code theme owned the token. The axis split
applied to a token users read as prose.

### 9.13 Mode-split fade strengths: light-mode gray legibility (2026-08-19)

User verdict: light mode's gray text ("tool name inside runs", the
worktree name under a thread, diff-view labels) is hard to read.
Measured, the complaint is structural: the derived fg tiers faded by
the same percentages in both modes (80/55/30% of `--text-primary`),
but alpha compositing is asymmetric: fading toward a light ground
loses contrast faster than toward a dark one. At the shared strengths:
muted 10.9:1 dark vs 8.6 light, subtle 5.6 vs **3.8** (below the 4.5
body floor), hint 2.4 vs **1.9**.

Fix at the derivation, not the ~300 call sites: the strengths moved
into per-mode `--fade-muted/subtle/hint` vars in app.css (`:root`
80/55/30%, `html.light` 87/67/39%, solved numerically to reproduce
dark's measured ratios), and tokens.css's tiers consume them
(`color-mix(in oklab, var(--text-primary) var(--fade-muted), …)`).
tokens.css's `html.light` block stays EMPTY (the pin behind
`isSharedDefaultToken` holds; the mode now rides `--fade-*` as well as
`--text-primary`), `--fade-` joins the drift test's excluded prefixes
(a percentage scale, not palette, since themes tune the tiers by
stating `fg-muted/subtle/hint`, never these), and the contrast suite's
derived-tier alpha table went per-mode. Every light theme that leaves
the tiers derived heals at once; themes stating opaque tiers (High
Contrast) are untouched.

### 9.14 Blacklight: first original curated palette (2026-08-20)

`blacklight` (dark-only, both axes) is the first curated theme with no
upstream: UV fluorescence (magenta / UV purple / cyan / mint) on a
violet-tinted near-black. It began as the author's user theme file
("neon") and was promoted verbatim, plus the `syntax-markup-list`
token that postdates the file. Two as-built notes:

- The §9.11 entry-at-the-bottom rule has nothing to re-anchor here,
  because there is no upstream range. The ladder is its own near-black
  ramp (`#000005 → #0d0d1c → #17172e → #222240`, steps 1.09–1.14:1) and the
  focal band tops the curated set (19.35:1).
- It states all three fade tiers, not just `fg-muted`. The derived
  fades over `#f5f5ff` drift gray, and the stated tiers keep the
  violet cast; `fg-hint` (#8a8ac0, 6.5:1) clears the stated-tier 4.5:1
  claim with room, so the §9.11 "state only fg-muted" guidance is a
  default, not a rule.
