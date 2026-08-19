// Tokyo Night — folke/tokyonight.nvim's "night" variant (palette read from
// `lua/tokyonight/colors/storm.lua` + `night.lua`, 2026-08-18). Dark-only,
// both axes.
//
// SURFACE LADDER. tokyonight is the one upstream here whose sidebar is DARKER
// than its editor, so the app ground takes bg_dark (#16161e) and the editor
// ground (#1a1b26) becomes the first elevation step — which also keeps
// `code-block` on the tier our own default puts it on. Storm's bg (#24283b)
// and night's bg_highlight (#292e42) complete the ramp; all four are
// published values.
//
// Palette: bg #1a1b26, bg_dark #16161e, storm's bg #24283b (used here as the
// inline-code ground), fg #c0caf5, fg_dark #a9b1d6, comment #565f89,
// dark5 #737aa2, red #f7768e, orange #ff9e64, yellow #e0af68, green #9ece6a,
// green1 #73daca, blue #7aa2f7, blue1 #2ac3de, blue5 #89ddff, cyan #7dcfff,
// magenta #bb9af7, terminal_black #414868.
//
// ROLE MAPPING notes where judgement was involved:
//
//   - `property` is green1/teal (#73daca) — tokyonight's field/property hue,
//     which is one of the few places it differs sharply from other dark
//     themes and is a large part of how it reads.
//   - `variable-builtin` and `tag` are red, `attribute` is magenta: the
//     theme's tag/attribute pair.
//   - `string-special` is blue5 (#89ddff), tokyonight's escape/special-char
//     hue, kept distinct from the magenta keyword rather than folded into it.
//   - `namespace` tracks `type` (blue1), the github default's convention.
//   - `markup-quote` is dark5 (#737aa2) rather than the comment grey, which
//     measures below 3:1 on this ground; block quotes carry prose.
//   - `syntax-comment` keeps the canonical #565f89 and its measured ratio is
//     pinned in `builtins.contrast.test.ts`.

import type { BuiltinThemeSpec } from '../builtins';

export const TOKYO_NIGHT: BuiltinThemeSpec = {
  id: 'tokyo-night',
  name: 'Tokyo Night',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#16161e',
      'surface-1': '#1a1b26',
      'surface-2': '#24283b',
      'surface-3': '#292e42',
      border: '#3b4261',
      'border-strong': '#414868',
      'text-primary': '#c0caf5',
      'text-secondary': '#a9b1d6',
      accent: '#7aa2f7',
      'accent-fg': '#16161e',
      // Markdown prose roles: this file's markup-* hues, so chat prose and
      // fenced markdown agree when both axes are on Tokyo Night. Bold is
      // the signature purple — Tokyo Night's emphasis hue beside its blue
      // headings. Inline-code text is code-axis (see the `code` section
      // below).
      'md-heading': '#7aa2f7',
      'md-bold': '#bb9af7',
      'md-link': '#73daca',
      'md-blockquote': '#737aa2',
      'md-marker': '#ff9e64',
      info: '#7dcfff',
      success: '#9ece6a',
      error: '#f7768e',
      warning: '#e0af68',
      overlay: '#0c0e14a6',
      'ico-terminal': '#73daca',
      'ico-file': '#bb9af7',
      'ico-eye': '#7dcfff',
      'ico-search': '#7aa2f7',
      'ico-globe': '#2ac3de',
      'ico-robot': '#9d7cd8',
      'ico-speech-bubble': '#f7768e',
      'ico-checklist': '#9ece6a',
      'ico-puzzle': '#c7a9ff',
      'ico-clock': '#e0af68',
      'ico-brain': '#ff9e64',
      'ico-compaction': '#737aa2',
    },
    syntax: {
      'syntax-keyword': '#bb9af7',
      'syntax-string': '#9ece6a',
      'syntax-string-special': '#89ddff',
      'syntax-comment': '#565f89',
      'syntax-number': '#ff9e64',
      'syntax-function': '#7aa2f7',
      'syntax-type': '#2ac3de',
      'syntax-variable-builtin': '#f7768e',
      'syntax-property': '#73daca',
      'syntax-constant': '#ff9e64',
      'syntax-tag': '#f7768e',
      'syntax-attribute': '#bb9af7',
      'syntax-namespace': '#2ac3de',
      'syntax-label': '#7aa2f7',
      'syntax-markup-heading': '#7aa2f7',
      'syntax-markup-link': '#73daca',
      'syntax-markup-raw': '#9ece6a',
      'syntax-markup-list': '#ff9e64',
      'syntax-markup-quote': '#737aa2',
      'syntax-added': '#9ece6a',
      'syntax-removed': '#f7768e',
    },
    // tokyonight's published terminal palette (its kitty/alacritty extras).
    ansi: {
      'ansi-fg-30': '#15161e',
      'ansi-fg-31': '#f7768e',
      'ansi-fg-32': '#9ece6a',
      'ansi-fg-33': '#e0af68',
      'ansi-fg-34': '#7aa2f7',
      'ansi-fg-35': '#bb9af7',
      'ansi-fg-36': '#7dcfff',
      'ansi-fg-37': '#a9b1d6',
      'ansi-fg-90': '#414868',
      'ansi-fg-91': '#ff899d',
      'ansi-fg-92': '#9fe044',
      'ansi-fg-93': '#faba4a',
      'ansi-fg-94': '#8db0ff',
      'ansi-fg-95': '#c7a9ff',
      'ansi-fg-96': '#a4daff',
      'ansi-fg-97': '#c0caf5',
    },
    code: {
      'code-block': '#1a1b26',
      'code-inline-bg': '#24283b',
      // Inline-code text beside its chip ground (markup-raw green).
      'md-inline-code': '#9ece6a',
      'terminal-bg': '#1a1b26',
    },
  },
};
