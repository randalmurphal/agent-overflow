// One Dark — Atom's One Dark, in the form One Dark Pro ships as its "Darker"
// variant (Binaryify/OneDark-Pro, `themes/OneDark-Pro-darker.json`, read
// 2026-08-18). Dark-only, so it renders as a dark island on a light UI.
//
// GROUND. The darker variant states `editor.background: #23272e` and
// `sideBar.background: #1e2227`; the deeper of the two is used here for the
// code block and the terminal, per an explicit request for the darker ground.
// `code-inline-bg` is its `editor.lineHighlightBackground` (#2c313c), which is
// exactly the "same ground, one step up" role an inline span wants.
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
  axes: { ui: false, code: true },
  dark: {
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
      'code-block': '#1e2227',
      'code-inline-bg': '#2c313c',
      'terminal-bg': '#1e2227',
    },
  },
};
