// Catppuccin — Mocha (dark) and Latte (light), from the official palette and
// the project's own syntax style guide
// (catppuccin/catppuccin `docs/style-guide.md`, read 2026-08-18):
//
//   Keywords → Mauve            Strings → Green
//   Escapes/regex → Pink        Comments → Overlay 2
//   Numbers/constants → Peach   Functions → Blue
//   Types/classes → Yellow      Builtins → Red
//   Properties → Blue           Attributes → Yellow
//   Parameters → Maroon         Enum variants → Teal
//   Markdown links → Blue       Diff add/remove → Green / Red
//
// The guide is silent on three of our families, so they are judgement, taken
// from the palette the guide does hand out:
//
//   - `tag` → Lavender. Blue is already carrying functions AND properties;
//     lavender keeps markup tag names legible as their own thing.
//   - `namespace` → Teal. Yellow already carries types and attributes.
//   - `label` → Maroon, the guide's parameter hue (named arguments are the
//     dominant use of this family).
//
// The guide closes with "legibility always comes first, so use your own
// judgement", which is what licenses the LATTE deviations below: five accents
// do not clear 3:1 on Latte's own base, so each is darkened minimally and the
// canonical value is recorded beside it.

import type { BuiltinThemeSpec } from '../builtins';

export const CATPPUCCIN: BuiltinThemeSpec = {
  id: 'catppuccin',
  name: 'Catppuccin',
  axes: { ui: false, code: true },
  // Mocha. base #1e1e2e, surface0 #313244, surface1 #45475a, surface2 #585b70,
  // overlay2 #9399b2, subtext1 #bac2de, text #cdd6f4.
  dark: {
    syntax: {
      'syntax-keyword': '#cba6f7',
      'syntax-string': '#a6e3a1',
      'syntax-string-special': '#f5c2e7',
      'syntax-comment': '#9399b2',
      'syntax-number': '#fab387',
      'syntax-function': '#89b4fa',
      'syntax-type': '#f9e2af',
      'syntax-variable-builtin': '#f38ba8',
      'syntax-property': '#89b4fa',
      'syntax-constant': '#fab387',
      'syntax-tag': '#b4befe',
      'syntax-attribute': '#f9e2af',
      'syntax-namespace': '#94e2d5',
      'syntax-label': '#eba0ac',
      'syntax-markup-heading': '#f38ba8',
      'syntax-markup-link': '#89b4fa',
      'syntax-markup-raw': '#a6e3a1',
      'syntax-markup-list': '#fab387',
      'syntax-markup-quote': '#9399b2',
      'syntax-added': '#a6e3a1',
      'syntax-removed': '#f38ba8',
    },
    // Catppuccin's terminal ports map black/white onto the surface and subtext
    // ramps and repeat the accents in the bright half; that is the palette as
    // published, not a gap. Slots 37 and 97 take subtext1 and text so ordinary
    // output reads as body copy.
    ansi: {
      'ansi-fg-30': '#45475a',
      'ansi-fg-31': '#f38ba8',
      'ansi-fg-32': '#a6e3a1',
      'ansi-fg-33': '#f9e2af',
      'ansi-fg-34': '#89b4fa',
      'ansi-fg-35': '#f5c2e7',
      'ansi-fg-36': '#94e2d5',
      'ansi-fg-37': '#bac2de',
      'ansi-fg-90': '#585b70',
      'ansi-fg-91': '#f38ba8',
      'ansi-fg-92': '#a6e3a1',
      'ansi-fg-93': '#f9e2af',
      'ansi-fg-94': '#89b4fa',
      'ansi-fg-95': '#f5c2e7',
      'ansi-fg-96': '#94e2d5',
      'ansi-fg-97': '#cdd6f4',
    },
    code: {
      'code-block': '#1e1e2e',
      'code-inline-bg': '#313244',
      'terminal-bg': '#1e1e2e',
    },
  },
  // Latte. base #eff1f5, surface0 #ccd0da, surface1 #bcc0cc, surface2 #acb0be,
  // overlay2 #7c7f93, subtext1 #5c5f77, text #4c4f69.
  //
  // Darkened for the 3:1 floor on base, canonical value in brackets:
  //   green    [#40a02b] → #3a8f27   (2.94:1 → 3.63:1)
  //   yellow   [#df8e1d] → #b3730f   (2.29:1 → 3.42:1)
  //   peach    [#fe640b] → #d95a05   (2.62:1 → 3.39:1)
  //   pink     [#ea76cb] → #cc4dad   (2.31:1 → 3.40:1)
  //   lavender [#7287fd] → #6b7ce8   (2.79:1 → 3.25:1)
  light: {
    syntax: {
      'syntax-keyword': '#8839ef',
      'syntax-string': '#3a8f27',
      'syntax-string-special': '#cc4dad',
      'syntax-comment': '#7c7f93',
      'syntax-number': '#d95a05',
      'syntax-function': '#1e66f5',
      'syntax-type': '#b3730f',
      'syntax-variable-builtin': '#d20f39',
      'syntax-property': '#1e66f5',
      'syntax-constant': '#d95a05',
      'syntax-tag': '#6b7ce8',
      'syntax-attribute': '#b3730f',
      'syntax-namespace': '#179299',
      'syntax-label': '#e64553',
      'syntax-markup-heading': '#d20f39',
      'syntax-markup-link': '#1e66f5',
      'syntax-markup-raw': '#3a8f27',
      'syntax-markup-list': '#d95a05',
      'syntax-markup-quote': '#7c7f93',
      'syntax-added': '#3a8f27',
      'syntax-removed': '#d20f39',
    },
    ansi: {
      'ansi-fg-30': '#5c5f77',
      'ansi-fg-31': '#d20f39',
      'ansi-fg-32': '#3a8f27',
      'ansi-fg-33': '#b3730f',
      'ansi-fg-34': '#1e66f5',
      'ansi-fg-35': '#cc4dad',
      'ansi-fg-36': '#179299',
      'ansi-fg-37': '#5c5f77',
      'ansi-fg-90': '#7c7f93',
      'ansi-fg-91': '#d20f39',
      'ansi-fg-92': '#3a8f27',
      'ansi-fg-93': '#b3730f',
      'ansi-fg-94': '#1e66f5',
      'ansi-fg-95': '#cc4dad',
      'ansi-fg-96': '#179299',
      'ansi-fg-97': '#4c4f69',
    },
    code: {
      'code-block': '#eff1f5',
      'code-inline-bg': '#ccd0da',
      'terminal-bg': '#eff1f5',
    },
  },
};
