// One Dark — Atom's One Dark, in the form One Dark Pro ships as its "Darker"
// variant (Binaryify/OneDark-Pro, `themes/OneDark-Pro-darker.json`, read
// 2026-08-18). Dark-only on BOTH axes, so its chrome is offered beside its
// code, and in light mode the code half still renders as a dark island.
//
// SURFACE LADDER, anchored on the same file's workbench colors but entered
// one step BELOW them. The darker variant runs a two-ground design —
// sidebar/tabs/status at #1e2227, editor and activity bar at #23272e — with
// #2c313a for list hover/selection and #404754 for buttons. Used verbatim
// that ladder compresses into an ~11:1 luminance range and the whole app
// reads hazy beside the default theme (~17:1); see the luminance-range rule
// in `docs/specs/theme-system.md` §9.11. So the app ground extends the ramp
// DOWN by its own
// step (#16191d — not a published One Dark value, same hue and chroma
// direction as #1e2227 → #23272e continued), the published grounds shift up
// one tier each, and the widest published step (#404754) keeps the top slot.
//
// GROUND. Code block and terminal sit on the extended #16191d app ground, so
// a code block reads as a well cut into the cards above it.
// `code-inline-bg` (UI axis, with the rest of the prose chrome) is its
// `editor.lineHighlightBackground` (#2c313c), which is exactly the
// "same ground, one step up" role an inline span wants.
//
// ROLE MAPPING. Our 21 families are not TextMate scopes, so the mapping is by
// ROLE, taken from the scopes the upstream file actually paints:
//
//   - `variable-builtin` is chalky (#e5c07b), not coral: One Dark Pro paints
//     `variable.language` (this/self/super) with the class hue, and following
//     the upstream is worth more than making the token look distinct.
//   - `property` is coral (#e06c75) — object property ACCESS. Upstream leaves
//     `support.type.property-name` (JSON/CSS keys) at the plain foreground;
//     one token cannot say both, and the access form is the one that dominates
//     code we render.
//   - `label` is coral (#e06c75), from `entity.name.label`.
//   - `namespace` is chalky, from `entity.name.namespace` — which also matches
//     the convention the github default uses (namespace tracks type).
//   - `markup-quote` is the COMMENT grey (#7f848e) rather than upstream's
//     #5c6370: that value measures 2.2:1 on this ground, and a block quote
//     carries prose, not decoration. Comments themselves keep their canonical
//     value (see `builtins.contrast.test.ts`).

import type { BuiltinThemeSpec } from '../builtins';

export const ONE_DARK: BuiltinThemeSpec = {
  id: 'one-dark',
  name: 'One Dark',
  axes: { ui: true, code: true },
  dark: {
    // Chrome. Focal text is #e6e6e6 — the theme's own ANSI bright-white,
    // its brightest published tone — rather than the editor foreground
    // #abb2bf or the mid tier #d7dae0 (ANSI 37): chat chrome body copy
    // renders at the fg-muted tier (80% of focal), so every step of focal
    // grayness is paid twice, and both dimmer choices left the whole app
    // reading gray beside the default theme's near-white. #abb2bf (the
    // `sideBar.foreground` mono-1) stays as supporting.
    // Borders are the published `panel.border` / `focusBorder` value with a
    // brighter step above it for the emphasized tier.
    colors: {
      'surface-0': '#16191d',
      'surface-1': '#1e2227',
      'surface-2': '#2c313a',
      'surface-3': '#404754',
      border: '#3e4452',
      'border-strong': '#5c6370',
      'text-primary': '#e6e6e6',
      'text-secondary': '#abb2bf',
      accent: '#61afef',
      'accent-fg': '#16191d',
      // Markdown prose roles: the same hues this file's code axis gives the
      // markup-* families, so chat prose and fenced markdown agree when both
      // axes are on One Dark. Bold is the number/attribute orange — One Dark
      // paints `markup.bold` with it.
      'md-heading': '#e06c75',
      'md-bold': '#d19a66',
      'md-link': '#61afef',
      'md-blockquote': '#7f848e',
      'md-marker': '#e5c07b',
      // Inline-code chip: lineHighlightBackground ground, markup-raw green
      // text (6.5:1).
      'code-inline-bg': '#2c313c',
      'md-inline-code': '#98c379',
      info: '#61afef',
      success: '#98c379',
      error: '#e06c75',
      warning: '#e5c07b',
      overlay: '#181a1fa6',
      // Thirteen icons over an eight-hue palette, so some hues repeat; the
      // terminal ANSI values (which are One Dark Pro's own) supply the rest.
      'ico-terminal': '#56b6c2',
      'ico-file': '#c678dd',
      'ico-eye': '#61afef',
      'ico-search': '#4aa5f0',
      'ico-globe': '#42b3c2',
      'ico-robot': '#c162de',
      'ico-speech-bubble': '#e06c75',
      'ico-checklist': '#98c379',
      'ico-puzzle': '#de73ff',
      'ico-clock': '#e5c07b',
      'ico-brain': '#ff616e',
      'ico-compaction': '#828997',
    },
    syntax: {
      'syntax-keyword': '#c678dd',
      'syntax-string': '#98c379',
      'syntax-string-special': '#56b6c2',
      'syntax-comment': '#7f848e',
      'syntax-number': '#d19a66',
      'syntax-function': '#61afef',
      'syntax-type': '#e5c07b',
      'syntax-variable-builtin': '#e5c07b',
      'syntax-property': '#e06c75',
      'syntax-constant': '#d19a66',
      'syntax-tag': '#e06c75',
      'syntax-attribute': '#d19a66',
      'syntax-namespace': '#e5c07b',
      'syntax-label': '#e06c75',
      'syntax-markup-heading': '#e06c75',
      'syntax-markup-link': '#61afef',
      'syntax-markup-raw': '#98c379',
      'syntax-markup-list': '#e5c07b',
      'syntax-markup-quote': '#7f848e',
      'syntax-added': '#98c379',
      'syntax-removed': '#e06c75',
    },
    // `terminal.ansi*` from the same file, verbatim.
    ansi: {
      'ansi-fg-30': '#3f4451',
      'ansi-fg-31': '#e05561',
      'ansi-fg-32': '#8cc265',
      'ansi-fg-33': '#d18f52',
      'ansi-fg-34': '#4aa5f0',
      'ansi-fg-35': '#c162de',
      'ansi-fg-36': '#42b3c2',
      'ansi-fg-37': '#d7dae0',
      'ansi-fg-90': '#4f5666',
      'ansi-fg-91': '#ff616e',
      'ansi-fg-92': '#a5e075',
      'ansi-fg-93': '#f0a45d',
      'ansi-fg-94': '#4dc4ff',
      'ansi-fg-95': '#de73ff',
      'ansi-fg-96': '#4cd1e0',
      'ansi-fg-97': '#e6e6e6',
    },
    code: {
      'code-block': '#16191d',
      'terminal-bg': '#16191d',
    },
  },
};
