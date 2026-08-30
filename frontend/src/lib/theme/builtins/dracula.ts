// Dracula — the official palette, UI shades and ANSI set from the Dracula spec
// and its reference VSCode theme (`dracula/visual-studio-code` `src/dracula.yml`,
// read 2026-08-18). Dark-only, both axes.
//
// Palette, for reading the mapping below: background #282a36, current line
// #44475a, foreground #f8f8f2, comment #6272a4, cyan #8be9fd, green #50fa7b,
// orange #ffb86c, pink #ff79c6, purple #bd93f9, red #ff5555, yellow #f1fa8c.
// UI variants: bgDarker #191a21, bgDark #21222c, bgLight #343746,
// bgLighter #424450.
//
// SURFACE LADDER uses those UI variants, entered from the BOTTOM of the
// published range rather than at the canonical background: bgDarker #191a21 →
// bgDark #21222c → background #282a36 → bgLight #343746. Anchoring at
// #282a36 compressed the app into an ~13:1 luminance range and it read hazy
// beside the default theme (~17:1); see the luminance-range rule in
// `docs/architecture/theme-system.md` §9.11. Every step is a published Dracula
// value — the canonical background becomes the card tier, which is where
// most content actually sits.
//
// BORDERS are the one place the upstream cannot be followed. Dracula's own
// border color is bgDarker (#191a21) — a line DARKER than its editor ground
// (and here the app ground itself), which works in an editor whose panels
// are separated by shadow but is invisible for a hairline vocabulary built
// on visible separation. The border tiers therefore take the comment tone
// #6272a4 (the palette's own mid value) and a lightened step of it for the
// emphasized tier.
//
// TEXT SECONDARY is likewise adapted: Dracula names no supporting-text tone
// other than the comment, and #6272a4 is the weakest tone in the palette, so
// the role takes a lightened step of the same hue (#7d8ab8).
//
// ROLE MAPPING notes where judgement was involved:
//
//   - `type` and `property` are both cyan. That is the spec: class names are
//     cyan (italic-underlined upstream, which we cannot express) and property
//     names are cyan too. Two roles, one hue, on purpose.
//   - `variable-builtin` is purple — the spec's language-variable rule.
//   - `label` is orange, the spec's parameter hue, which is the closest role
//     to a named argument or a jump target.
//   - `namespace` tracks `type`, the convention the github default uses.
//   - `string-special` is red, from the spec's regex rule.

import type { BuiltinThemeSpec } from '../builtins';

export const DRACULA: BuiltinThemeSpec = {
  id: 'dracula',
  name: 'Dracula',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#191a21',
      'surface-1': '#21222c',
      'surface-2': '#282a36',
      'surface-3': '#343746',
      border: '#6272a4',
      'border-strong': '#7d8ab8',
      'text-primary': '#f8f8f2',
      'text-secondary': '#7d8ab8',
      accent: '#bd93f9',
      'accent-fg': '#191a21',
      // Markdown prose roles: this file's markup-* hues, so chat prose and
      // fenced markdown agree when both axes are on Dracula. Bold is the
      // signature pink — Dracula's spec renders bold in pink alongside
      // purple headings. The quote deviates from markup-quote (#6272a4)
      // to the same lightened comment step text-secondary already uses:
      // a block quote carries prose, and the lighter step keeps it
      // clearly readable while the fenced markup-quote keeps the
      // canonical comment tone. Inline-code text is code-axis (see the
      // `code` section below).
      'md-heading': '#bd93f9',
      'md-bold': '#ff79c6',
      'md-link': '#8be9fd',
      'md-blockquote': '#7d8ab8',
      'md-marker': '#ffb86c',
      // Inline-code chip: currentLine ground, markup-raw yellow text.
      'code-inline-bg': '#44475a',
      'md-inline-code': '#f1fa8c',
      info: '#8be9fd',
      success: '#50fa7b',
      error: '#ff5555',
      warning: '#ffb86c',
      overlay: '#191a21a6',
      'ico-terminal': '#8be9fd',
      'ico-file': '#bd93f9',
      'ico-eye': '#a4ffff',
      'ico-search': '#d6acff',
      'ico-globe': '#8be9fd',
      'ico-robot': '#bd93f9',
      'ico-speech-bubble': '#ff79c6',
      'ico-checklist': '#50fa7b',
      'ico-puzzle': '#ff92df',
      'ico-clock': '#f1fa8c',
      'ico-brain': '#ffb86c',
      'ico-compaction': '#7d8ab8',
    },
    syntax: {
      'syntax-keyword': '#ff79c6',
      'syntax-string': '#f1fa8c',
      'syntax-string-special': '#ff5555',
      'syntax-comment': '#6272a4',
      'syntax-number': '#bd93f9',
      'syntax-function': '#50fa7b',
      'syntax-type': '#8be9fd',
      'syntax-variable-builtin': '#bd93f9',
      'syntax-property': '#8be9fd',
      'syntax-constant': '#bd93f9',
      'syntax-tag': '#ff79c6',
      'syntax-attribute': '#50fa7b',
      'syntax-namespace': '#8be9fd',
      'syntax-label': '#ffb86c',
      'syntax-markup-heading': '#bd93f9',
      'syntax-markup-link': '#8be9fd',
      'syntax-markup-raw': '#f1fa8c',
      'syntax-markup-list': '#ffb86c',
      'syntax-markup-quote': '#6272a4',
      'syntax-added': '#50fa7b',
      'syntax-removed': '#ff5555',
    },
    // The spec's terminal ANSI table, verbatim.
    ansi: {
      'ansi-fg-30': '#21222c',
      'ansi-fg-31': '#ff5555',
      'ansi-fg-32': '#50fa7b',
      'ansi-fg-33': '#f1fa8c',
      'ansi-fg-34': '#bd93f9',
      'ansi-fg-35': '#ff79c6',
      'ansi-fg-36': '#8be9fd',
      'ansi-fg-37': '#f8f8f2',
      'ansi-fg-90': '#6272a4',
      'ansi-fg-91': '#ff6e6e',
      'ansi-fg-92': '#69ff94',
      'ansi-fg-93': '#ffffa5',
      'ansi-fg-94': '#d6acff',
      'ansi-fg-95': '#ff92df',
      'ansi-fg-96': '#a4ffff',
      'ansi-fg-97': '#ffffff',
    },
    code: {
      // Code sits on the extended-down app ground (bgDarker), which also
      // lifts the canonical comment #6272a4 from 3.0:1 to 4.0:1.
      'code-block': '#191a21',
      'terminal-bg': '#191a21',
    },
  },
};
