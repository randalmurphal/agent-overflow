# Theme System — Surface Survey

Status: **investigation + settled direction** (2026-08-18). Every
themeable surface mapped; §6 records the user's decisions, §7 the
reworked architecture, §8 the remaining open items.

Goal: VSCode-level user customization — colors for every part of the UI
(text hierarchy, headers/bold in markdown, icons, backgrounds, sidebar,
borders), predefined popular themes for code surfaces, and a
configuration format an AI agent can be pointed at and edit sanely.

## 1. What exists today

The app already has a real semantic token system. Theming is **not** an
extraction job; it is closing ~35 leak sites and putting a user-editable
layer on top of an existing vocabulary.

### 1.1 The three token files

| File | Owns |
|---|---|
| `frontend/src/app.css` | ALL colors: the `:root` dark palette, the single `html.light` override block, and the theme-INVARIANT color roles that deliberately have no light counterpart (`--provider-claude`, `--scrim`/`--scrim-fg`, `--design-paper`) — so every literal color in the app is readable in one place. Plus the `@theme` mapping (`--color-*` → Tailwind utilities like `bg-surface-1`, `text-fg-muted`) |
| `frontend/src/styles/tokens.css` | Non-color scales: type (`--text-micro..lg`), radius (`--radius-field/control/card/composer`), shadows (`--shadow-sheet/menu/modal`) — plus DERIVED color roles, which are `var()`/`color-mix()` over what app.css defined and so need no light override of their own: the fg hierarchy (`--fg-muted/subtle/hint`), `--border-subtle`, `--card`, `--code-block` |
| `frontend/src/styles/syntax.css` | 21 `--syntax-*` vars (github-dark `:root`, github-light `html.light`) + the `.syntax-<name>` rules |

Derived tokens (`--fg-*`, `--border-subtle`, all three shadows, the
usage heat ramp) are `color-mix()` off base tokens — they follow a base
palette change automatically. A theme only has to set base tokens.

### 1.2 Token families (the theme vocabulary)

- **Surfaces**: `--surface-0/1/2/3`, `--card` and `--code-block`
  (aliases over surface-1), `--terminal-bg`, `--code-inline-bg`,
  `--overlay` (dims app chrome behind a modal — theme-varying)
- **On-media**: `--scrim` / `--scrim-fg` — chrome painted over user
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
  not byte-identical" with the terminal — see §3.1)
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
  read by our CSS; `.dark` exists for the vendored streamdown's
  MutationObserver), idempotent against the live DOM so no-op applies
  don't wake that observer.
- Applied from `App.svelte`, alongside `applyFonts` and `applyFontScale`.
  The theme one is an `$effect.pre`: App's pre-effect stamps the class
  ahead of every descendant effect, so a consumer that reads resolved
  colors out of the DOM (the mermaid bridge's probe) never samples the
  outgoing palette on a flip. The font/zoom appliers stay plain
  `$effect`s — nothing reads their values back.
- No FOUC guard: `index.html` has no pre-hydration script, so a
  light-theme user gets a dark first frame.

### 1.4 The runtime-override precedent: `utils/fonts.ts`

Settings → Appearance fonts already do runtime token override, and the
pattern is the one to copy: `setProperty('--font-sans', …)` on `<html>`
only when the user picked a non-default; `removeProperty` at the default
so the cascade (`@theme` in app.css) stays the single source of truth.
Memoized (`lastSans/lastMono`); lazy CSS import for Hack Nerd.
`utils/zoom.ts` is the same shape for `fontSize`.

**Perf constraint for a live theme editor**: every
`setProperty` on `<html>` invalidates style for the entire document
(~13ms @ 5k nodes, ~90ms @ 30k — measured, see `utils/ambientTicker.ts`
header, which exists specifically to avoid root custom-prop writes). N
tokens applied as N `setProperty` calls during a color-picker drag is a
jank machine. Apply a user theme as **one `<style id="user-theme">`
element rewrite** (single `:root {…} html.light {…}` rule block) —
one invalidation per change, and "absent = cascade default" falls out
naturally.

## 2. Code/syntax pipeline — why code themes are nearly free

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
inline; `ambientTicker` writes only opacity/scalars/transform. So a
runtime palette change repaints everything for free **except**:

| Consumer | Today | For custom palettes |
|---|---|---|
| **xterm terminals** (`terminal/terminalTheme.ts`, applied in `terminalXterm.ts`, `TerminalBody.svelte`, `TakeControlTerminal.svelte`) | Hand-maintained hex `DARK`/`LIGHT` `ITheme` objects (44 values) — xterm rejects `oklch`, so CSS vars are NOT read; the app.css comment claiming otherwise is stale. `$effect` re-applies on `getResolvedTheme()` flip; live `term.options.theme` write works. | Effect must track palette identity, not just light/dark. Either resolve `--ansi-*`/`--terminal-bg` at runtime or make the theme format carry a terminal section in hex. The runtime route must REUSE `utils/cssColorProbe.ts` — the module the phase-1 mermaid bridge already resolves token colors through; do not grow a second resolver (see §7 phase 2). WebGL addon has a GPU glyph-atlas cache — verify visually. |
| **Mermaid** (`StreamdownMermaidHost.svelte` + vendored `Streamdown.svelte:52`) | **BRIDGED (phase 1).** Was: built-in `'dark'`/`'default'` palettes only, diagram fills/strokes/labels a wholly uncaptured surface. Now `theme:'base'` + `themeVariables` resolved from app tokens (`chat/markdown/mermaidTokens.ts`, via the probe in `utils/cssColorProbe.ts`); the `{#key}` and the vendored SVG cache key both key on the palette. | Widen the palette identity when theme files land — see §7 phase 2. |
| **Native window background** (`main_desktop.go:78,169`) | `NewRGBA(22,22,30)` hardcoded at construction — the resize-flash color; wrong for light theme already. | New Go binding (window bg is settable post-creation in Wails v3), called from App's theme effect alongside `applyThemeClass`. Classify in `LocalOnlyMethods` per transport rules. |
| **WSL launcher pages** (`cmd/agent-overflow-windows/picker.go`) | Inline HTML, hardcoded `#16161e` + Tokyo Night. Pre-app bootstrap; could at best read settings JSON off disk for light/dark. | Low priority. |
| **Favicon** (`public/favicon.svg`) | Static, dark tile. | Optional `prefers-color-scheme` inside the SVG. |
| **Design canvas iframes** | Opaque-origin sandbox; agent HTML owns its colors. `bg-white` paper on the host side. | Out of scope architecturally (sandbox posture). Tokenize the paper as `--design-paper`, default locked white. |
| **Second window / `--connect` client** | No `settings:changed` push event exists — another client learns of a theme change only on reload. | Needs a push event if themes should sync live (template: `app_workflow_definitions_watcher.go`'s `a.emit`). |
| **First frame** | No FOUC guard in `index.html`. | Pre-hydration script or Wails-side class stamp. |

KaTeX: `currentColor`, nothing to do. Streamdown markdown theme
(`chat/markdown/streamdownTheme.ts`): static token-class strings, fine.

## 4. Leak inventory (~115 literals, ~35 edit sites)

> **This section is the PRE-phase-1 inventory, and it is now CLOSED**
> (landed 2026-08-18 — see §7). It is kept as the historical record of
> what leaked and why each fix was chosen; line numbers and "needed"
> framing describe the tree as it was surveyed, not as it is. What keeps
> the state clean going forward is `src/lib/themeTokens.test.ts`, whose
> allowlists are shrink-only. Where the shipped design diverged from the
> proposal below, the divergence is noted inline.

Full details in the categories below; everything else swept clean
(usage, accounts, git, palette, import, discussion, panes, run-map SVGs,
primitives icons).

### 4.1 New token roles needed

1. **`--accent-fg`** (fg-on-accent) — highest leverage. 4 live
   `bg-accent text-white` sites (`WorkflowActionRow.svelte:195,229,253`,
   `ReviewLineBlockRow.svelte:111`) + `settings/styles.ts:23`'s
   `text-accent-foreground` which is a **dead class** today (no
   `--color-accent-foreground` exists — primary settings buttons render
   with ambient text color on the accent fill). A user-picked pale
   accent breaks all of these without this token.
2. **`--surface-3`** — 2 dead-class sites
   (`ProviderAccountLimits.svelte:32`, `TerminalTabStrip.svelte:146`
   use `bg-surface-3` which compiles to nothing). Define it or rewrite
   to `surface-2`.
3. **Scrim family** — image lightbox (`ExpandedImageDialog.svelte`):
   `bg-black/88` + `white`-based controls (~14 occurrences). Wants
   `--overlay-strong` + `--scrim-fg` (near-black scrim is correct in
   BOTH themes, so it must be its own role, not `--overlay`).
4. **On-media badges** — `bg-black/70 text-white` over image thumbnails
   (`UserMessage.svelte:170`, `ComposerAttachmentRow.svelte:81,87`).
   `--badge-on-media-{bg,fg}` or reuse the scrim pair.

   **Shipped differently (3 + 4 folded into one pair).** Both are the
   same statement — chrome painted over USER MEDIA — so instead of
   `--overlay-strong` plus a `--badge-on-media-*` family, phase 1 landed
   ONE theme-invariant pair, `--scrim` / `--scrim-fg`, opaque black and
   white, consumed with Tailwind opacity modifiers (`bg-scrim/88`,
   `text-scrim-fg/78`, `ring-scrim-fg/70`). Storing them undiluted is
   what lets one pair serve every dilution the lightbox and the badges
   each wanted. They are deliberately distinct from `--overlay`, which
   dims APP CHROME behind a modal and therefore does vary by theme —
   the vocabulary note lives at both definitions in `app.css`.
5. **`--provider-claude`** — retires `text-[#d97757]`
   (`ProviderIcon.svelte:28`), completes the provider family. Brand
   colors stay default-locked but become visible to a theme editor.
6. *(optional)* `--shadow-accent-inset` (dup'd arbitrary shadow in
   `ComposerPendingApprovalPanel.svelte:36` /
   `ComposerPendingUserInputPanel.svelte:345` — already token-derived,
   just unnamed), `--design-paper`.

### 4.2 Straight swaps to existing tokens

- `bg-black/45` scrims → `bg-overlay`: `primitives/Modal.svelte:134`,
  `primitives/OverlayShell.svelte:40` (`DiagramModal.svelte:242` already
  does it right). Note `Modal.test.ts:232` pins the literal.
- `text-red-400` ×2 → `text-error`: `settings/ArchivedThreads.svelte:104,136`.
- Default Tailwind shadows → token shadows (10 sites — the 10th,
  `shared/ToggleSwitch.svelte:29`, was missed by this survey and caught
  by the phase-1 tripwire test): `shadow-sm` →
  `shadow-sheet` in `MessageTimeline.svelte:969`,
  `ProposedPlanCard.svelte:91`, `ComposerAttachmentRow.svelte:56`,
  `McpServersMenu.svelte:228`, `ThreadRow.svelte:466`,
  `WorkflowRunMap.svelte:295`; `shadow-lg` → `shadow-menu` in
  `LazyOverlay.svelte:44` and `ThreadRow.svelte:276` (JS-built drag
  ghost `className` — invisible to class scanners);
  `shadow-[0_1px_4px_rgba(0,0,0,0.25)]` → `shadow-sheet` in
  `ReviewDiffBody.svelte:390` (the only raw `rgba()` in src);
  `shadow-[var(--shadow-sheet)]` → `shadow-sheet` in `Segmented.svelte:52`.

**Three of these were NOT value-preserving, and shipped as accepted
visual changes** (the rest are exact swaps):

1. **Modal / OverlayShell backdrops**: `black/45` → `--overlay`, i.e.
   60% black in dark mode and a 28% light wash in light mode. The
   backdrop got denser in dark mode and changed character in light mode
   — deliberately, because it unifies the two shells with
   `DiagramModal`, which already used `bg-overlay`. Three modal
   surfaces disagreeing on backdrop density was the bug.
2. **`ReviewDiffBody` sticky-header shadow**: `0 1px 4px /25%` →
   `--shadow-sheet` (`0 1px 2px /20%`). Slightly tighter and lighter.
   Accepted: the point of the shadow scale is that one bespoke value
   does not get to sit half a step off the role it belongs to.
3. **Settings primary buttons**: `settings/styles.ts`'s
   `text-accent-foreground` was a DEAD class (no such token existed), so
   the label rendered in ambient text color on the accent fill. Pointing
   it at `text-accent-fg` gives it the white label it always intended —
   a visible change, and a fix rather than a regression.

### 4.3 Vendored streamdown keys that reach the screen unthemed

`chatMarkdownTheme` (`streamdownTheme.ts`) overrides most of the vendor
base theme, but 9 colored keys leak vendor palette classes
(`vendor/svelte-streamdown/dist/theme.js`): `alert.note/tip/warning/
caution/important` (GFM `> [!NOTE]` etc. → `text-blue-600`-family),
`del.base` (`~~strike~~` → `text-gray-600`, likely in real agent
prose), `footnoteRef.base`, `descriptionTerm/Detail.base`. Fix is 9
entries in `streamdownTheme.ts` mapping onto `--info/success/warning/
error/accent` + `--fg-subtle/hint` + `--card`/`--border-subtle` — no
new tokens. (`code.header/skeleton/…` and `inlineCitation.*` verified
unreachable; vendored `Image.svelte`'s gray chip unreachable with
`ALLOWED_IMAGE_PREFIXES=['*']`.)

### 4.4 Deliberate literals — keep, and mark as such

`imageCompress.ts:110` + vendored `MermaidDownload.svelte:118` (white
mattes on exported artifacts), `UserMessageBody.svelte:142` (mask alpha
channel, not a color), design-panel `bg-white` iframe paper, the brand
coral **value** (tokenize the name, lock the default).

### 4.5 Tests pinning colors

`terminalTheme.test.ts` (exact hex), `ProviderIcon.test.ts` /
`ModelProviderMenu.test.ts` (`text-[#d97757]`), `Modal.test.ts`
(`bg-black/45`), `internal/settings/validate_test.go` (asserts theme
`"solarized"` is rejected — any new theme id must extend
`allowedThemes` or the whole selection model changes),
`workflowRunMapStyle.test.ts` (exactly-one `--success` use — safe under
recoloring, breaks under token renames).

## 5. Settings/persistence facts that shape the design

- Settings = `<configDir>/settings.json`, **sparse-written** (only
  non-default keys), atomic rename, mtime-based lazy reload in
  `Service.Get()` — an external hand-edit IS picked up backend-side
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
  renames — a theme watcher must not feed back on its own writes).
- Adding a flat setting costs 4 sync points today (Go struct, Go
  defaults, TS interface, TS `DEFAULT_SETTINGS`) + allow-list + section
  UI. Another argument for one `theme` reference + separate theme files
  over N flat color keys.
- Scope: every appearance setting today is app-global. The per-client
  `ui_state` precedent exists (pane layout), but mixing would be novel.

## 6. Decisions (user, 2026-08-18)

1. **Two axes**: UI theme × code theme, independently selectable.
2. **Separate theme files**, not settings.json keys.
3. **Per-client scope.** Stated explicitly: a client will (future) be
   able to connect to multiple backends at the same time, and different
   clients may carry different theme configs. Theme is a property of
   the client machine, NOT of a backend — so it must not live in
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
  process registers **no services** — the SPA RPCs entirely against the
  remote backend; the local process is a static stub
  (`internal/clientmode`) that injects `window.__AO_BOOTSTRAP__` into
  index.html. But the client binary DOES have a durable per-machine
  ClientID (`ensureClientID`) and its own `<configDir>` on the client
  machine. So client-side theme files are *storable* there today, but
  there is no live channel from the stub to the webview — theme data
  would ride the bootstrap injection (applied at page load; live
  file-watch reload needs a small stub endpoint later).
- Pure-browser remote sessions: no local process, no files. Built-in
  themes + localStorage selection only.

This staging is the price of decision 3 and is acceptable: the primary
surface (desktop app) gets the full feature; `--connect` gets themes at
load; browsers get built-ins.

### 6.2 Consequences for existing settings

`settings.theme` (and arguably `sansFont`/`monoFont`/`fontSize` — the
same per-client appearance class; the projector case) migrates out of
backend settings into the client-side appearance config. Backend fields
retire via `retiredSettingsFieldNames()`, with a one-time read of the
old value into the client file at first boot. Recommended scoping: move
`theme` in phase 2; leave fonts/fontSize backend-global initially and
migrate in a follow-up (they have working plumbing and the split, while
philosophically inconsistent, costs nothing until multi-backend lands).

## 7. Architecture (per decisions)

### Phase 1 — close the leaks (decision-free, can start now)

Add the §4.1 tokens, do the §4.2 swaps, add the §4.3 streamdown keys,
bridge mermaid `themeVariables`, fix the two dead-class families,
collapse the duplicate `matchMedia` resolvers, fix the stale app.css
comment about terminalTheme. Update the pinned tests. End state:
**100% of app chrome resolves through the token vocabulary**, plus a
tripwire test that greps for raw palette classes / literals so the
state stays clean.

**Landed 2026-08-18.** What shipped: the §4.1 tokens (`--accent-fg`,
`--surface-3`, the `--scrim`/`--scrim-fg` pair that folded items 3+4,
`--provider-claude`, `--shadow-accent-inset`, `--code-block`,
`--design-paper`); the §4.2 swaps, three of them accepted visual changes
as noted there; the nine §4.3 streamdown keys; the mermaid
`themeVariables` bridge (`chat/markdown/mermaidTokens.ts` +
`utils/cssColorProbe.ts`, palette-keyed remount and SVG cache); the
resolver collapse to one `getResolvedTheme` owning the single
`matchMedia` subscription, with App stamping the class from an
`$effect.pre`; the stale app.css comment about `terminalTheme` fixed;
and the tripwire, `src/lib/themeTokens.test.ts`, whose raw-class
allowlist is EMPTY — that emptiness is the phase-1 completion claim.
Still open from this phase's wish list: the FOUC guard and the native
window background, both carried into phase 2 below.

### Phase 2 — theme files, selection, live reload (desktop)

- **Files**: `<configDir>/themes/*.json` on the client machine. Each
  file: `{ "$schema": …, "name", "extends": "dark"|"light",
  "colors": {surface,text,border,accent,status,provider,icons,…},
  "syntax": {…}, "ansi": {…}, "terminal": {…} }` — sparse overrides
  over the extended base. Agent discoverability comes from the JSON
  schema + a generated `themes/TOKENS.md` reference listing every
  token, its role, and its default per base — NOT from materializing
  every value into every file (materialized files go stale the moment
  the app grows a token; `extends` inherits new tokens automatically).
- **Selection** (user-confirmed model, 2026-08-18): client-side
  appearance config (same dir), `{ mode: system|light|dark,
  uiTheme: id, codeTheme: id }`. ONE theme per axis; a theme FILE may
  define a `light` palette, a `dark` palette, or both (the built-in
  default defines both — exactly today's `:root` + `html.light` pair),
  and `mode` picks which palette renders. Missing-variant rules differ
  by axis, deliberately: a dark-only UI theme in light mode falls back
  to the default light palette (chrome must match the mode); a
  dark-only CODE theme stays itself in light mode, self-contained —
  code surfaces own their backgrounds (`--code-block`,
  `--code-inline-bg`, `--terminal-bg`), so a Monokai block on a light
  UI renders as a dark island, the familiar docs-site pattern, instead
  of unreadable dark-on-light text. The `html.light` class flip stays
  pure CSS.
- **Application**: resolve both axes into ONE
  `<style id="user-theme">` rewrite containing `:root {…}` +
  `html.light {…}` override blocks (single style invalidation — §1.4
  perf note; absent token = cascade default, the fonts.ts principle).
  A single resolved-appearance store carries palette identity; the
  xterm effects and mermaid `{#key}`/cache key track it.
- **Terminal hex bridge**: resolve token values to hex/rgb for xterm,
  replacing the hand-maintained `DARK`/`LIGHT` duplicates in
  `terminalTheme.ts`. **Reuse `utils/cssColorProbe.ts`** — the module
  phase 1's mermaid bridge already does this through; do not write a
  second resolver. Its mechanism is not the obvious one, which is why
  the module exists: `getComputedStyle` does NOT normalize colors to
  `rgb()`. Computed styles serialize in their DECLARED color space, so
  an `oklch()` token reads back as the `oklch()`/`oklab()` string, which
  xterm rejects. Resolution therefore needs the probe element AND a
  1×1-canvas readback (paint the color, read the pixel) to land on real
  channel values. `mermaidTokens.browser.test.ts` pins that behavior in
  a real browser — a jsdom test cannot see it, which is precisely how
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

### Phase 3 — code-theme bundles + remote clients

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

1. **Selection shape**: confirmed as §7 phase 2 — one theme per axis,
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
