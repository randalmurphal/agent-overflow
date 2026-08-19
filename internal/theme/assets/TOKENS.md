# Theme tokens

<!-- GENERATED FILE — do not edit.
     Source: frontend/src/lib/theme/tokenRegistry.ts + the app stylesheets.
     Regenerate: cd frontend && node scripts/generate-theme-reference.mjs -->

Every color this app paints, and the name a theme file calls it by. 85 tokens: 46 in `colors`, 21 in `syntax`, 16 in `ansi`, 2 in `code`.

## Where theme files live

```
<configDir>/themes/
  appearance.json      the selection: mode, uiTheme, codeTheme
  my-theme.json        a theme; its id is the filename stem ("my-theme")
  theme.schema.json    this file's machine-readable twin (generated)
  TOKENS.md            this file (generated)
```

Edit a file and the app reloads it — no restart. A file that is broken does
not break the app: what could not be understood is reported as a warning and
skipped, per token, and everything else still applies.

## The format

```json
{
  "$schema": "./theme.schema.json",
  "name": "Display Name",
  "dark":  { "colors": {}, "syntax": {}, "ansi": {}, "code": {} },
  "light": { "colors": {} }
}
```

- **Sparse, and there is no `extends`.** A variant block overrides the
  built-in palette **of its own polarity** — that is what `"dark"` and
  `"light"` MEAN here. Name only the tokens you are changing; everything you
  leave out keeps the built-in value, including tokens added by a future
  version of the app. A materialized copy of every value would go stale the
  day a token is added; a sparse file never does.
- **One theme per axis.** The UI axis (`colors`) and the code axis
  (`syntax` + `ansi` + `code`) are selected independently, so a UI theme
  and a code theme can come from different files. A file that defines
  `colors` in any variant is offered on the UI axis; a file that defines any
  code section is offered on the code axis; one file may serve both.
- **Missing variants behave differently per axis, on purpose.**
  - *UI axis*: chrome must match the mode. A theme with only a `dark` block
    applies in dark mode and steps aside entirely in light mode, where the
    built-in light palette renders instead.
  - *Code axis*: block-code surfaces own their own grounds (`code-block`,
    `terminal-bg`), so a theme with only a `dark` block **stays itself in
    light mode** — a dark code island on a light page, the familiar docs-site
    pattern, rather than unreadable dark-on-light text. Inline code chips in
    prose are UI-axis (`code-inline-bg` + `md-inline-code` in `colors`)
    and follow the UI theme like the rest of the prose chrome.
- **Built-in ids are `default` (UI) and `github` (code).** Both are
  identity themes: they name the palette the app ships with rather than
  restating it. A file of your own with one of those names shadows the
  built-in.
- **Values are CONCRETE CSS colors**, as a string — any color syntax the
  browser accepts, `color-mix()` included. Up to 256
  characters, and restricted to the characters a color value needs, so a value
  cannot carry a second declaration or a comment. A value that is not a valid
  color is skipped with a warning; the rest of the theme still applies.
  `url()`, `image-set()` and `src()` are refused — a palette needs no
  network fetch — and so are `var()`, `attr()` and `env()`: a reference
  that does not resolve blanks every property that reads it, which would let
  one bad token take out the whole app ground instead of costing one color.
  "Follow another token" is what the derived rows below already do.
- **Derived tokens follow their base.** The rows marked **Derived** below
  default to an expression over another token — the foreground hierarchy over
  the text color, the subtle border over the border, the card and code-block
  grounds over the first elevation tier. Move the base and they all move with
  it. Override one only when you want it to stop tracking.
- **A token with no light default reaches both modes.** The rows showing
  **—** in the light column have no light-mode declaration to out-cascade the
  dark one, so in a file that states BOTH `"dark"` and `"light"`, naming
  one of them in only one block still paints it in both. State it in both
  blocks when you want two colors; leave it in one when one color is what you
  meant. Either way the app says which tokens you did that to.

## Not themeable

| Variable | Why |
| --- | --- |
| `--radius-*` | radius scale — geometry, not palette |
| `--shadow-*` | shadow roles — already derived from the palette by color-mix |
| `--font-*` | font stacks — owned by Settings → Appearance, not by theme files |
| `--color-*` | the @theme mapping layer, which re-exports tokens as utilities |
| `--animate-*` | animation registrations, not colors |
| `--run-map-*` | run-map lane geometry (lengths) |
| `--text-micro` | type scale — shares the --text- stem with the text colors, so it is excluded by name |
| `--text-xs` | type scale |
| `--text-sm` | type scale |
| `--text-base` | type scale |
| `--text-lg` | type scale |

These are structure rather than palette. Shadows are already mixed from the
palette, and letting a theme move radii or type sizes turns "pick a palette"
into "rebuild the layout".

## Tokens

A **—** in the light column means the token has no light-mode declaration: the
dark value applies in both modes (for a derived token, it re-derives from
whatever base the mode has).

### `colors` — UI axis

| Token | What it paints | Dark default | Light default |
| --- | --- | --- | --- |
| `surface-0` | App ground: the window and page background every pane sits on. | `oklch(0.145 0.014 285.82)` | `oklch(0.985 0.004 255)` |
| `surface-1` | First elevation step: cards, inputs and panels lifted off the ground. | `oklch(0.178 0.014 285.82)` | `oklch(0.955 0.008 255)` |
| `surface-2` | Second elevation step: chrome sitting on a card (menus, chips, hover fills). | `oklch(0.215 0.014 285.82)` | `oklch(0.915 0.012 255)` |
| `surface-3` | Third elevation step: progress tracks and hover fills on chrome already at tier 2. | `oklch(0.25 0.014 285.82)` | `oklch(0.875 0.016 255)` |
| `border` | Default hairline between regions. | `oklch(0.3 0.014 285.82)` | `oklch(0.84 0.014 255)` |
| `border-strong` | Prominent hairline: turn dividers and hover-emphasized control borders. | `oklch(0.52 0.014 285.82)` | `oklch(0.74 0.016 255)` |
| `border-subtle` | Softest hairline, for ambient chrome. Follows the border color. **Derived** — override only to stop it tracking. | `color-mix(in oklab, var(--border) 55%, transparent)` | — |
| `text-primary` | Focal text. | `oklch(0.95 0.006 285.82)` | `oklch(0.23 0.014 255)` |
| `text-secondary` | Supporting text: secondary labels, quoted prose, muted glyphs. | `oklch(0.7 0.006 285.82)` | `oklch(0.48 0.018 255)` |
| `fg-muted` | Body copy. Follows the focal text color at reduced strength. **Derived** — override only to stop it tracking. | `color-mix(in oklab, var(--text-primary) 80%, transparent)` | — |
| `fg-subtle` | Labels and de-emphasized text. Follows the focal text color. **Derived** — override only to stop it tracking. | `color-mix(in oklab, var(--text-primary) 55%, transparent)` | — |
| `fg-hint` | Timestamps and barely-there hints. Follows the focal text color. **Derived** — override only to stop it tracking. | `color-mix(in oklab, var(--text-primary) 30%, transparent)` | — |
| `accent` | Primary accent: selection, links, focus rings and filled buttons. | `oklch(0.58 0.19 276)` | `oklch(0.55 0.18 276)` |
| `accent-fg` | Foreground painted on an accent fill, so a pale accent cannot strand its own label. | `#ffffff` | — |
| `md-heading` | Markdown headings in chat prose (the code-axis counterpart is syntax-markup-heading). Follows the focal text color. **Derived** — override only to stop it tracking. | `var(--text-primary)` | — |
| `md-bold` | Markdown bold text in chat prose. No code-axis counterpart exists, so curated themes pick an emphasis hue of their own. Follows the focal text color. **Derived** — override only to stop it tracking. | `var(--text-primary)` | — |
| `md-link` | Markdown links in chat prose (the code-axis counterpart is syntax-markup-link). Follows the accent. **Derived** — override only to stop it tracking. | `var(--accent)` | — |
| `md-blockquote` | Markdown block-quote text in chat prose (the code-axis counterpart is syntax-markup-quote). Follows the supporting text color. **Derived** — override only to stop it tracking. | `var(--text-secondary)` | — |
| `md-marker` | Markdown list bullets and numbers in chat prose (the code-axis counterpart is syntax-markup-list). Follows the muted body-text tier (fg-muted). **Derived** — override only to stop it tracking. | `var(--fg-muted)` | — |
| `code-inline-bg` | Ground behind inline code spans in prose. | `oklch(0.275 0.014 285.82)` | `oklch(0.9 0.012 255)` |
| `md-inline-code` | Markdown inline-code text, painted on the code-inline-bg chip (the highlight counterpart is syntax-markup-raw). Both chip tokens are UI-axis: the chip is monochrome prose furniture, not a highlighted surface, and text and ground stay on ONE axis so no UI/code combination can split the pair. Follows the focal text color. **Derived** — override only to stop it tracking. | `var(--text-primary)` | — |
| `info` | Informational status: input prompts and neutral notices. | `oklch(0.62 0.17 250)` | `oklch(0.55 0.17 250)` |
| `success` | Success status: completed work, added lines, healthy state. | `oklch(0.7 0.17 150)` | `oklch(0.5 0.15 155)` |
| `error` | Failure status: errors, removed lines, refusals. | `oklch(0.63 0.2 25)` | `oklch(0.5 0.18 27)` |
| `warning` | Attention status: a human is blocked (approvals, pending input). | `oklch(0.75 0.15 75)` | `oklch(0.6 0.16 78)` |
| `provider-codex` | Codex provider identity. Brand-locked by default. | `oklch(0.72 0.15 55)` | `oklch(0.58 0.16 55)` |
| `provider-claude` | Claude provider identity. Brand-locked by default, with no separate light value. | `#d97757` | — |
| `provider-claude-tui` | claude-tui provider identity: the phosphor green of a CRT terminal. | `oklch(0.82 0.2 145)` | `oklch(0.55 0.16 145)` |
| `overlay` | Backdrop dimming app chrome behind a modal or sheet. Varies with the mode. | `oklch(0 0 0 / 60%)` | `oklch(0.22 0.01 255 / 28%)` |
| `scrim` | Chrome painted over USER MEDIA (lightbox grounds, thumbnail badges). Stored opaque and consumed at partial alpha; mode-invariant by default. | `oklch(0 0 0)` | — |
| `scrim-fg` | Foreground of the media-overlay pair. Stored opaque and consumed at partial alpha; mode-invariant by default. | `oklch(1 0 0)` | — |
| `design-paper` | The design canvas iframe paper. Sandboxed agent HTML is authored against a light page, so this is default-locked. | `#ffffff` | — |
| `card` | Ambient card ground, referenced at low alpha by tool rows and cards. Follows the first elevation tier. **Derived** — override only to stop it tracking. | `var(--surface-1)` | — |
| `ico-terminal` | Tool-kind icon: shell and terminal commands. | `oklch(0.78 0.12 195)` | `oklch(0.5 0.12 195)` |
| `ico-file` | Tool-kind icon: file edits and writes. | `oklch(0.72 0.16 305)` | `oklch(0.5 0.18 305)` |
| `ico-eye` | Tool-kind icon: reads and views. | `oklch(0.78 0.1 220)` | `oklch(0.5 0.1 220)` |
| `ico-search` | Tool-kind icon: search and pattern matching. | `oklch(0.74 0.13 240)` | `oklch(0.48 0.15 240)` |
| `ico-globe` | Tool-kind icon: network and web fetches. | `oklch(0.78 0.12 215)` | `oklch(0.5 0.12 215)` |
| `ico-robot` | Tool-kind icon: subagents and delegated work. | `oklch(0.72 0.16 280)` | `oklch(0.5 0.18 280)` |
| `ico-speech-bubble` | Tool-kind icon: conversation and messaging tools. | `oklch(0.76 0.14 330)` | `oklch(0.52 0.16 330)` |
| `ico-checklist` | Tool-kind icon: task lists and plans. | `oklch(0.76 0.12 255)` | `oklch(0.5 0.12 255)` |
| `ico-puzzle` | Tool-kind icon: MCP servers and plugin tools. | `oklch(0.74 0.14 325)` | `oklch(0.5 0.16 325)` |
| `ico-clock` | Tool-kind icon: waits, sleeps and scheduled work. | `oklch(0.76 0.1 90)` | `oklch(0.5 0.12 90)` |
| `ico-brain` | Tool-kind icon: thinking and reasoning. | `oklch(0.76 0.14 15)` | `oklch(0.52 0.16 15)` |
| `ico-compaction` | Tool-kind icon: context compaction. A muted slate that reads as a system operation. | `oklch(0.72 0.07 265)` | `oklch(0.5 0.09 265)` |
| `ico-generic` | Tool-kind icon fallback for unclassified tools. Follows the secondary text color. **Derived** — override only to stop it tracking. | `var(--text-secondary)` | `var(--text-secondary)` |

### `syntax` — code axis

| Token | What it paints | Dark default | Light default |
| --- | --- | --- | --- |
| `syntax-keyword` | Language keywords and control flow. | `#ff7b72` | `#cf222e` |
| `syntax-string` | String literals. | `#a5d6ff` | `#0a3069` |
| `syntax-string-special` | Escapes, interpolation markers and regex literals. | `#79c0ff` | `#0550ae` |
| `syntax-comment` | Comments and doc comments. | `#8b949e` | `#6e7781` |
| `syntax-number` | Numeric literals. | `#79c0ff` | `#0550ae` |
| `syntax-function` | Function and method names at definition and call sites. | `#d2a8ff` | `#8250df` |
| `syntax-type` | Types, classes, structs and interfaces. | `#ffa657` | `#953800` |
| `syntax-variable-builtin` | Built-in variables such as this, self and super. | `#ff7b72` | `#cf222e` |
| `syntax-property` | Object properties and struct fields. | `#79c0ff` | `#0550ae` |
| `syntax-constant` | Constants and enum members, including the language booleans. | `#79c0ff` | `#0550ae` |
| `syntax-tag` | Markup tag names (HTML, JSX, XML). | `#7ee787` | `#116329` |
| `syntax-attribute` | Markup attribute names and annotations. | `#79c0ff` | `#0550ae` |
| `syntax-namespace` | Modules, packages and namespace qualifiers. | `#ffa657` | `#953800` |
| `syntax-label` | Labels, goto targets and named arguments. | `#79c0ff` | `#0550ae` |
| `syntax-markup-heading` | Markdown headings (also rendered bold). | `#79c0ff` | `#0550ae` |
| `syntax-markup-link` | Markdown links and URLs. | `#a5d6ff` | `#0a3069` |
| `syntax-markup-raw` | Markdown inline code and fenced blocks. | `#a5d6ff` | `#0a3069` |
| `syntax-markup-list` | Markdown list markers. | `#ffa657` | `#953800` |
| `syntax-markup-quote` | Markdown block quotes. | `#8b949e` | `#6e7781` |
| `syntax-added` | Added lines in an embedded diff. | `#7ee787` | `#116329` |
| `syntax-removed` | Removed lines in an embedded diff. | `#ff7b72` | `#cf222e` |

### `ansi` — code axis

| Token | What it paints | Dark default | Light default |
| --- | --- | --- | --- |
| `ansi-fg-30` | ANSI 30 (black) foreground, in chat output and the terminal alike. | `#4b5563` | `#24292f` |
| `ansi-fg-31` | ANSI 31 (red) foreground, in chat output and the terminal alike. | `oklch(0.7 0.17 27)` | `#cf222e` |
| `ansi-fg-32` | ANSI 32 (green) foreground, in chat output and the terminal alike. | `oklch(0.75 0.15 150)` | `#116329` |
| `ansi-fg-33` | ANSI 33 (yellow) foreground, in chat output and the terminal alike. | `oklch(0.82 0.14 85)` | `#7d4e00` |
| `ansi-fg-34` | ANSI 34 (blue) foreground, in chat output and the terminal alike. | `oklch(0.72 0.14 250)` | `#0550ae` |
| `ansi-fg-35` | ANSI 35 (magenta) foreground, in chat output and the terminal alike. | `oklch(0.75 0.16 320)` | `#8250df` |
| `ansi-fg-36` | ANSI 36 (cyan) foreground, in chat output and the terminal alike. | `oklch(0.8 0.12 205)` | `#1b7c83` |
| `ansi-fg-37` | ANSI 37 (white) foreground. Follows the app text palette unless overridden. **Derived** — override only to stop it tracking. | `var(--text-primary)` | `var(--text-primary)` |
| `ansi-fg-90` | ANSI 90 (bright black, i.e. grey) foreground. Follows the app text palette unless overridden. **Derived** — override only to stop it tracking. | `var(--text-secondary)` | `var(--text-secondary)` |
| `ansi-fg-91` | ANSI 91 (bright red) foreground, in chat output and the terminal alike. | `oklch(0.78 0.15 27)` | `#a40e26` |
| `ansi-fg-92` | ANSI 92 (bright green) foreground, in chat output and the terminal alike. | `oklch(0.82 0.14 150)` | `#1a7f37` |
| `ansi-fg-93` | ANSI 93 (bright yellow) foreground, in chat output and the terminal alike. | `oklch(0.88 0.13 85)` | `#633c01` |
| `ansi-fg-94` | ANSI 94 (bright blue) foreground, in chat output and the terminal alike. | `oklch(0.8 0.13 250)` | `#0969da` |
| `ansi-fg-95` | ANSI 95 (bright magenta) foreground, in chat output and the terminal alike. | `oklch(0.82 0.15 320)` | `#6f42c1` |
| `ansi-fg-96` | ANSI 96 (bright cyan) foreground, in chat output and the terminal alike. | `oklch(0.86 0.11 205)` | `#3192aa` |
| `ansi-fg-97` | ANSI 97 (bright white) foreground, in chat output and the terminal alike. | `#ffffff` | `#1f2328` |

### `code` — code axis

| Token | What it paints | Dark default | Light default |
| --- | --- | --- | --- |
| `code-block` | Ground behind a fenced code block. Follows the first elevation tier, so a code theme can move blocks without moving every card with them. **Derived** — override only to stop it tracking. | `var(--surface-1)` | — |
| `terminal-bg` | Ground of the embedded terminal. | `#000000` | `#fafafb` |
