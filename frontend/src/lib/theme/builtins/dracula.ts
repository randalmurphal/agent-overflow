// Dracula — the official palette and ANSI set from the Dracula spec
// (draculatheme.com/contribute, "Color Palette" + "Syntax Highlighting").
// Dark-only.
//
// Palette, for reading the mapping below: background #282a36, current line
// #44475a, foreground #f8f8f2, comment #6272a4, cyan #8be9fd, green #50fa7b,
// orange #ffb86c, pink #ff79c6, purple #bd93f9, red #ff5555, yellow #f1fa8c.
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
  axes: { ui: false, code: true },
  dark: {
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
      'code-block': '#282a36',
      'code-inline-bg': '#44475a',
      'terminal-bg': '#282a36',
    },
  },
};
