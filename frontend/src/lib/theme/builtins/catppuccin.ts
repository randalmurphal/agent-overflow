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
// Both axes. The UI half is where Catppuccin fits our vocabulary best: it is
// the one upstream that publishes a named FOUR-step elevation ramp, so the
// surface ladder is its own, verbatim and in order —
//
//   Mocha  base #1e1e2e → surface0 #313244 → surface1 #45475a → surface2 #585b70
//   Latte  base #eff1f5 → mantle  #e6e9ef → surface0 #ccd0da → surface1 #bcc0cc
//
// Latte enters through mantle rather than jumping straight to surface0: on a
// light ground the base→surface0 step is a visible slab, and mantle is the
// tone the flavour publishes for exactly that secondary-chrome role. Borders
// take the overlay ramp, text the text/subtext ramp, and the accent is Mauve
// (Catppuccin's own default accent).
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
  axes: { ui: true, code: true },
  // Mocha. base #1e1e2e, surface0 #313244, surface1 #45475a, surface2 #585b70,
  // overlay0 #6c7086, overlay1 #7f849c, overlay2 #9399b2, subtext0 #a6adc8,
  // subtext1 #bac2de, text #cdd6f4.
  dark: {
    colors: {
      'surface-0': '#1e1e2e',
      'surface-1': '#313244',
      'surface-2': '#45475a',
      'surface-3': '#585b70',
      border: '#6c7086',
      'border-strong': '#7f849c',
      'text-primary': '#cdd6f4',
      'text-secondary': '#a6adc8',
      accent: '#cba6f7',
      'accent-fg': '#1e1e2e',
      // Markdown prose roles: this file's markup-* hues (red headings,
      // blue links, peach markers), so chat prose and fenced markdown agree
      // when both axes are on Catppuccin. Bold is yellow — the warm
      // emphasis pastel beside the peach markers. Inline-code text is
      // code-axis (see the `code` section below).
      'md-heading': '#f38ba8',
      'md-bold': '#f9e2af',
      'md-link': '#89b4fa',
      'md-blockquote': '#9399b2',
      'md-marker': '#fab387',
      info: '#89b4fa',
      success: '#a6e3a1',
      error: '#f38ba8',
      warning: '#f9e2af',
      overlay: '#11111ba6',
      // Fourteen published accents against thirteen icon roles, so every one
      // of these is a distinct upstream color.
      'ico-terminal': '#94e2d5',
      'ico-file': '#cba6f7',
      'ico-eye': '#89dceb',
      'ico-search': '#89b4fa',
      'ico-globe': '#74c7ec',
      'ico-robot': '#b4befe',
      'ico-speech-bubble': '#f5c2e7',
      'ico-checklist': '#a6e3a1',
      'ico-puzzle': '#f2cdcd',
      'ico-clock': '#f9e2af',
      'ico-brain': '#eba0ac',
      'ico-compaction': '#9399b2',
    },
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
      // Inline-code text beside its chip ground (markup-raw green).
      'md-inline-code': '#a6e3a1',
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
    colors: {
      'surface-0': '#eff1f5',
      'surface-1': '#e6e9ef',
      'surface-2': '#ccd0da',
      'surface-3': '#bcc0cc',
      border: '#9ca0b0',
      'border-strong': '#7c7f93',
      'text-primary': '#4c4f69',
      'text-secondary': '#6c6f85',
      accent: '#8839ef',
      'accent-fg': '#eff1f5',
      // Latte counterparts of the mocha prose roles. A two-variant theme
      // must state these in BOTH variants or not at all — the md-* defaults
      // are declared once for both modes, so an omission here would leave
      // mocha's dark values painting on the latte ground (the resolver's
      // mode-invariant warning covers exactly this). Where latte has a
      // text-grade hue the role gets it (red headings, canonical blue
      // links, darkened peach markers, muted quotes); yellow measures
      // 2.3:1 on this ground — far under the 4:1 body floor even darkened —
      // so bold restates the latte text color and weight carries the
      // emphasis. Inline-code text is code-axis (see the `code` section
      // below).
      'md-heading': '#d20f39',
      'md-bold': '#4c4f69',
      'md-link': '#1e66f5',
      'md-blockquote': '#7c7f93',
      'md-marker': '#d95a05',
      info: '#1e66f5',
      success: '#3a8f27',
      error: '#d20f39',
      warning: '#b3730f',
      overlay: '#4c4f6947',
      'ico-terminal': '#179299',
      'ico-file': '#8839ef',
      'ico-eye': '#209fb5',
      'ico-search': '#1e66f5',
      'ico-globe': '#04a5e5',
      'ico-robot': '#7287fd',
      'ico-speech-bubble': '#ea76cb',
      'ico-checklist': '#40a02b',
      'ico-puzzle': '#dd7878',
      'ico-clock': '#df8e1d',
      'ico-brain': '#e64553',
      'ico-compaction': '#7c7f93',
    },
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
      // Inline-code text on the chip: latte's darkened green measures 2.7:1
      // on this ground — under the 4:1 chip floor — so the role restates the
      // latte text color; the chip ground and the mono face carry the
      // code-ness.
      'md-inline-code': '#4c4f69',
      'terminal-bg': '#eff1f5',
    },
  },
};
