// Gruvbox — morhetz's retro groove palette, dark and light. Both axes.
//
//   dark    dark0 #282828  dark0_soft #32302f  dark1 #3c3836  dark2 #504945
//           dark3 #665c54  dark4 #7c6f64       gray #928374
//   light   light0 #fbf1c7 light0_soft #f2e5bc light1 #ebdbb2 light2 #d5c4a1
//           light3 #bdae93 light4 #a89984
//   bright  red #fb4934  green #b8bb26  yellow #fabd2f  blue #83a598
//           purple #d3869b  aqua #8ec07c  orange #fe8019
//   neutral red #cc241d  green #98971a  yellow #d79921  blue #458588
//           purple #b16286  aqua #689d6a  orange #d65d0e
//   faded   red #9d0006  green #79740e  yellow #b57614  blue #076678
//           purple #8f3f71  aqua #427b58  orange #af3a03
//
// Gruvbox publishes a full ground ramp per mode PLUS a hard-contrast ground
// on each end (dark0_hard #1d2021, light0_hard #f9f5d7), so both surface
// ladders are upstream values with nothing invented — and both run in
// gruvbox's own HARD CONTRAST pairing (`contrast = hard`: bg0_hard with
// fg0), which is what keeps the app crisp where the soft pairing read hazy
// (see the luminance-range rule in `docs/architecture/theme-system.md` §9.11):
//
//   dark   dark0_hard #1d2021 → dark0 #282828 → dark1 #3c3836 → dark2 #504945
//          with fg0 #fbf1c7 focal over fg4 #a89984 supporting
//   light  light0_hard #f9f5d7 → light0_soft #f2e5bc → light1 #ebdbb2 →
//          light2 #d5c4a1, with fg0 #282828 focal over fg4 #7c6f64 supporting
//
// The palette's three intensity bands are what the two variants draw their
// accents from — bright on dark, faded on light — which is exactly how
// gruvbox itself flips.
//
// ROLE MAPPING follows morhetz's vim highlight groups: Statement/Keyword red,
// String green, Function green, Type yellow, Constant/Number purple,
// Identifier blue, PreProc aqua, Special orange. From those:
//
//   - `property` is blue (Identifier), `tag` aqua (PreProc), `namespace`
//     tracks `type` at yellow, `label` takes the Special orange.
//   - `variable-builtin` is red, with Statement — gruvbox treats language
//     builtins as keywords rather than as variables.
//
// LEGIBILITY DEVIATIONS:
//
//   - LIGHT accent: gruvbox light has no accent that can carry a label on a
//     fill (faded yellow #b57614 tops out at 3.8:1 against light0), so the
//     accent role takes faded blue #076678 — still upstream, and 5.9:1 with
//     light0 as its foreground.
//   - LIGHT ANSI slot 37 takes dark3 #665c54 rather than the canonical light4
//     #7c6f64 (4.33:1 → 5.85:1). Slot 37 is what our renderer paints ordinary
//     output with, and gruvbox-light's terminal scheme assumes a separately
//     configured foreground. For the same reason slot 30 takes dark0 rather
//     than the canonical light0, which on this island would be the ground.

import type { BuiltinThemeSpec } from '../builtins';

export const GRUVBOX: BuiltinThemeSpec = {
  id: 'gruvbox',
  name: 'Gruvbox',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#1d2021',
      'surface-1': '#282828',
      'surface-2': '#3c3836',
      'surface-3': '#504945',
      border: '#665c54',
      'border-strong': '#7c6f64',
      'text-primary': '#fbf1c7',
      'text-secondary': '#a89984',
      accent: '#fabd2f',
      'accent-fg': '#1d2021',
      // Markdown prose roles from this file's markup-* hues (agreeing with
      // fenced markdown when both axes are on Gruvbox), and bold shares the
      // orange gruvbox reserves for emphasis. Inline-code text is code-axis
      // (see the `code` section below).
      'md-heading': '#b8bb26',
      'md-bold': '#fe8019',
      'md-link': '#83a598',
      'md-blockquote': '#928374',
      'md-marker': '#fe8019',
      // Inline-code chip: bg1 ground; aqua text per gruvbox.vim
      // (`markdownCode` → GruvboxAqua), reading apart from green headings.
      'code-inline-bg': '#32302f',
      'md-inline-code': '#8ec07c',
      info: '#83a598',
      success: '#b8bb26',
      error: '#fb4934',
      warning: '#fe8019',
      overlay: '#1d2021a6',
      'ico-terminal': '#8ec07c',
      'ico-file': '#d3869b',
      'ico-eye': '#83a598',
      'ico-search': '#458588',
      'ico-globe': '#8ec07c',
      'ico-robot': '#d3869b',
      'ico-speech-bubble': '#fe8019',
      'ico-checklist': '#b8bb26',
      'ico-puzzle': '#b16286',
      'ico-clock': '#fabd2f',
      'ico-brain': '#fb4934',
      'ico-compaction': '#a89984',
    },
    syntax: {
      'syntax-keyword': '#fb4934',
      'syntax-string': '#b8bb26',
      'syntax-string-special': '#fe8019',
      'syntax-comment': '#928374',
      'syntax-number': '#d3869b',
      'syntax-function': '#b8bb26',
      'syntax-type': '#fabd2f',
      'syntax-variable-builtin': '#fb4934',
      'syntax-property': '#83a598',
      'syntax-constant': '#d3869b',
      'syntax-tag': '#8ec07c',
      'syntax-attribute': '#fabd2f',
      'syntax-namespace': '#fabd2f',
      'syntax-label': '#fe8019',
      'syntax-markup-heading': '#b8bb26',
      'syntax-markup-link': '#83a598',
      'syntax-markup-raw': '#b8bb26',
      'syntax-markup-list': '#fe8019',
      'syntax-markup-quote': '#928374',
      'syntax-added': '#b8bb26',
      'syntax-removed': '#fb4934',
    },
    // gruvbox's published terminal palette: neutral band normal, bright band
    // bright, gray and light1 at the ends.
    ansi: {
      'ansi-fg-30': '#282828',
      'ansi-fg-31': '#cc241d',
      'ansi-fg-32': '#98971a',
      'ansi-fg-33': '#d79921',
      'ansi-fg-34': '#458588',
      'ansi-fg-35': '#b16286',
      'ansi-fg-36': '#689d6a',
      'ansi-fg-37': '#a89984',
      'ansi-fg-90': '#928374',
      'ansi-fg-91': '#fb4934',
      'ansi-fg-92': '#b8bb26',
      'ansi-fg-93': '#fabd2f',
      'ansi-fg-94': '#83a598',
      'ansi-fg-95': '#d3869b',
      'ansi-fg-96': '#8ec07c',
      'ansi-fg-97': '#ebdbb2',
    },
    code: {
      'code-block': '#1d2021',
      'terminal-bg': '#1d2021',
    },
  },
  light: {
    colors: {
      'surface-0': '#f9f5d7',
      'surface-1': '#f2e5bc',
      'surface-2': '#ebdbb2',
      'surface-3': '#d5c4a1',
      border: '#bdae93',
      'border-strong': '#a89984',
      'text-primary': '#282828',
      'text-secondary': '#7c6f64',
      accent: '#076678',
      'accent-fg': '#f9f5d7',
      // Light-mode counterparts of the dark prose roles, from the faded
      // ramp: green headings, orange bold/markers, blue links — the same
      // role → hue mapping as dark. Inline-code text is code-axis (see the
      // `code` section below).
      'md-heading': '#79740e',
      'md-bold': '#af3a03',
      'md-link': '#076678',
      'md-blockquote': '#7c6f64',
      'md-marker': '#af3a03',
      // Inline-code chip: light1 ground. Faded aqua #427b58 measures 3.64:1
      // on it — under the 4:1 chip floor, and gruvbox's faded ramp has no
      // darker aqua — so the text restates the light text color (the chip
      // ground and the mono face carry the code-ness), matching the
      // latte / solarized-light treatment.
      'code-inline-bg': '#ebdbb2',
      'md-inline-code': '#282828',
      info: '#076678',
      success: '#79740e',
      error: '#9d0006',
      warning: '#af3a03',
      overlay: '#28282847',
      'ico-terminal': '#427b58',
      'ico-file': '#8f3f71',
      'ico-eye': '#076678',
      'ico-search': '#458588',
      'ico-globe': '#427b58',
      'ico-robot': '#8f3f71',
      'ico-speech-bubble': '#af3a03',
      'ico-checklist': '#79740e',
      'ico-puzzle': '#b16286',
      'ico-clock': '#b57614',
      'ico-brain': '#9d0006',
      'ico-compaction': '#7c6f64',
    },
    syntax: {
      'syntax-keyword': '#9d0006',
      'syntax-string': '#79740e',
      'syntax-string-special': '#af3a03',
      'syntax-comment': '#7c6f64',
      'syntax-number': '#8f3f71',
      'syntax-function': '#79740e',
      'syntax-type': '#b57614',
      'syntax-variable-builtin': '#9d0006',
      'syntax-property': '#076678',
      'syntax-constant': '#8f3f71',
      'syntax-tag': '#427b58',
      'syntax-attribute': '#b57614',
      'syntax-namespace': '#b57614',
      'syntax-label': '#af3a03',
      'syntax-markup-heading': '#79740e',
      'syntax-markup-link': '#076678',
      'syntax-markup-raw': '#79740e',
      'syntax-markup-list': '#af3a03',
      'syntax-markup-quote': '#7c6f64',
      'syntax-added': '#79740e',
      'syntax-removed': '#9d0006',
    },
    ansi: {
      'ansi-fg-30': '#282828',
      'ansi-fg-31': '#cc241d',
      'ansi-fg-32': '#98971a',
      'ansi-fg-33': '#d79921',
      'ansi-fg-34': '#458588',
      'ansi-fg-35': '#b16286',
      'ansi-fg-36': '#689d6a',
      'ansi-fg-37': '#665c54',
      'ansi-fg-90': '#928374',
      'ansi-fg-91': '#9d0006',
      'ansi-fg-92': '#79740e',
      'ansi-fg-93': '#b57614',
      'ansi-fg-94': '#076678',
      'ansi-fg-95': '#8f3f71',
      'ansi-fg-96': '#427b58',
      'ansi-fg-97': '#3c3836',
    },
    code: {
      'code-block': '#f9f5d7',
      'terminal-bg': '#f9f5d7',
    },
  },
};
