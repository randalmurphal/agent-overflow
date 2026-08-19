// High Contrast — the one built-in that serves BOTH axes, and the one place
// where overriding DERIVED tokens directly is correct rather than lazy.
//
// The dark variant is built for an OLED panel in a bright room: `surface-0` is
// true black (#000000, which an OLED simply does not light), the elevation
// ladder steps in clear, separated near-blacks rather than the default's
// gentle 0.145 → 0.25 lightness walk, and every foreground is pushed up the
// ramp. The light variant is its mirror on paper white, for the same room with
// the lights on.
//
// WHY THE DERIVED TOKENS ARE OVERRIDDEN. The app's `--fg-muted/subtle/hint`
// are `color-mix()` fades of the primary text color at 80/55/30%, and
// `--border-subtle` is the border at 55%. Those fades are the DEFAULT theme's
// whole aesthetic — ambient chrome that recedes. A high-contrast theme that
// only moved the base palette would inherit the recession and undo itself:
// `fg-hint` at 30% of white on black measures about 3:1, and a subtle border
// at 55% of a faint border is very nearly nothing. So this theme states all
// four tiers as literal, opaque values. They stay a hierarchy — muted reads
// above subtle reads above hint — the hierarchy just runs between 12:1 and
// 21:1 instead of between 3:1 and 19:1. Everything else that derives
// (`card`, `ico-generic`, the shadow roles) is deliberately left alone: those
// follow the base palette correctly.
//
// Borders are the other deliberate loudness. `border` is a mid grey that is
// unmistakably a line rather than a suggestion, `border-strong` is close to
// the text ramp, and `border-subtle` — the softest of the three — is still
// held at a visible step rather than a fade toward the ground.
//
// The code sections are a high-luminance-spread palette rather than a port of
// anyone's theme: hues are chosen for separation at high lightness (pink,
// green, amber, blue, violet, cyan) so families stay distinguishable to a
// reader who needs the contrast in the first place.

import type { BuiltinThemeSpec } from '../builtins';

export const HIGH_CONTRAST: BuiltinThemeSpec = {
  id: 'high-contrast',
  name: 'High Contrast',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#000000',
      'surface-1': '#121216',
      'surface-2': '#1f1f26',
      'surface-3': '#2c2c36',
      border: '#7a7a8c',
      'border-strong': '#c8c8d8',
      'border-subtle': '#63636f',
      'text-primary': '#ffffff',
      'text-secondary': '#dcdce6',
      'fg-muted': '#f2f2f7',
      'fg-subtle': '#d8d8e2',
      'fg-hint': '#b4b4c2',
      accent: '#6cb6ff',
      'accent-fg': '#000000',
      info: '#7ec4ff',
      success: '#4ee88a',
      error: '#ff7b72',
      warning: '#ffc857',
      // Near-opaque so app chrome behind a modal stops competing with it.
      overlay: '#000000eb',
      'ico-terminal': '#63e6e2',
      'ico-file': '#e88bff',
      'ico-eye': '#8fd0ff',
      'ico-search': '#7fb6ff',
      'ico-globe': '#6fdcf5',
      'ico-robot': '#b39aff',
      'ico-speech-bubble': '#ff9ae0',
      'ico-checklist': '#9db8ff',
      'ico-puzzle': '#ff9ce8',
      'ico-clock': '#ffd166',
      'ico-brain': '#ff9c9c',
      'ico-compaction': '#b8c4dc',
    },
    syntax: {
      'syntax-keyword': '#ff8ecb',
      'syntax-string': '#7ff0a0',
      'syntax-string-special': '#ffd75f',
      'syntax-comment': '#b0b8c8',
      'syntax-number': '#ffb066',
      'syntax-function': '#7fd0ff',
      'syntax-type': '#ffe066',
      'syntax-variable-builtin': '#ffa0a0',
      'syntax-property': '#a0e8ff',
      'syntax-constant': '#ffb066',
      'syntax-tag': '#ff8ecb',
      'syntax-attribute': '#ffe066',
      'syntax-namespace': '#ffe066',
      'syntax-label': '#c0aaff',
      'syntax-markup-heading': '#7fd0ff',
      'syntax-markup-link': '#a0e8ff',
      'syntax-markup-raw': '#7ff0a0',
      'syntax-markup-list': '#ffb066',
      'syntax-markup-quote': '#b0b8c8',
      'syntax-added': '#7ff0a0',
      'syntax-removed': '#ffa0a0',
    },
    // ANSI 30 is deliberately NOT black. On a true-black terminal ground the
    // canonical value is invisible, and "invisible" is the one thing this
    // theme exists to refuse; it takes a mid grey that still reads as the
    // darkest slot of the ramp.
    ansi: {
      'ansi-fg-30': '#8a8a99',
      'ansi-fg-31': '#ff6b6b',
      'ansi-fg-32': '#4ee88a',
      'ansi-fg-33': '#ffc857',
      'ansi-fg-34': '#6cb6ff',
      'ansi-fg-35': '#ff8ecb',
      'ansi-fg-36': '#5fe3e3',
      'ansi-fg-37': '#ffffff',
      'ansi-fg-90': '#b0b8c8',
      'ansi-fg-91': '#ff9d9d',
      'ansi-fg-92': '#8cffb0',
      'ansi-fg-93': '#ffe08a',
      'ansi-fg-94': '#a0d0ff',
      'ansi-fg-95': '#ffb3e0',
      'ansi-fg-96': '#9af5f5',
      'ansi-fg-97': '#ffffff',
    },
    code: {
      'code-block': '#0a0a0d',
      'code-inline-bg': '#1a1a20',
      'terminal-bg': '#000000',
    },
  },
  light: {
    colors: {
      'surface-0': '#ffffff',
      'surface-1': '#f2f2f6',
      'surface-2': '#e2e2ea',
      'surface-3': '#d2d2dc',
      border: '#5a5a68',
      'border-strong': '#14141a',
      'border-subtle': '#8a8a99',
      'text-primary': '#000000',
      'text-secondary': '#24242e',
      'fg-muted': '#0d0d12',
      'fg-subtle': '#2e2e38',
      'fg-hint': '#55555f',
      accent: '#0b4fd6',
      'accent-fg': '#ffffff',
      info: '#0b4fd6',
      success: '#0a6b2e',
      error: '#ba1414',
      warning: '#7a4a00',
      overlay: '#000000a8',
      'ico-terminal': '#00646e',
      'ico-file': '#7a1fa2',
      'ico-eye': '#0a5a8c',
      'ico-search': '#1240b0',
      'ico-globe': '#045f7a',
      'ico-robot': '#5a2fc0',
      'ico-speech-bubble': '#a01070',
      'ico-checklist': '#23439c',
      'ico-puzzle': '#9c1470',
      'ico-clock': '#6b4a00',
      'ico-brain': '#a01f2e',
      'ico-compaction': '#3c4a66',
    },
    syntax: {
      'syntax-keyword': '#9b1069',
      'syntax-string': '#0a5f22',
      'syntax-string-special': '#7a4a00',
      'syntax-comment': '#3f4a5c',
      'syntax-number': '#8a3a00',
      'syntax-function': '#0a4fa8',
      'syntax-type': '#6b4a00',
      'syntax-variable-builtin': '#a01020',
      'syntax-property': '#0a5a7a',
      'syntax-constant': '#8a3a00',
      'syntax-tag': '#9b1069',
      'syntax-attribute': '#6b4a00',
      'syntax-namespace': '#6b4a00',
      'syntax-label': '#4a2fb0',
      'syntax-markup-heading': '#0a4fa8',
      'syntax-markup-link': '#0a5a7a',
      'syntax-markup-raw': '#0a5f22',
      'syntax-markup-list': '#8a3a00',
      'syntax-markup-quote': '#3f4a5c',
      'syntax-added': '#0a5f22',
      'syntax-removed': '#a01020',
    },
    // Mirror of the dark table: the white/bright-white slots become the dark
    // end of the ramp, because those are what ordinary output is painted with.
    ansi: {
      'ansi-fg-30': '#14141a',
      'ansi-fg-31': '#b3141e',
      'ansi-fg-32': '#0a6b2e',
      'ansi-fg-33': '#7a4a00',
      'ansi-fg-34': '#0b4fd6',
      'ansi-fg-35': '#9b1069',
      'ansi-fg-36': '#00646e',
      'ansi-fg-37': '#14141a',
      'ansi-fg-90': '#4a4a56',
      'ansi-fg-91': '#8f0f18',
      'ansi-fg-92': '#075223',
      'ansi-fg-93': '#5c3800',
      'ansi-fg-94': '#073ba6',
      'ansi-fg-95': '#760c50',
      'ansi-fg-96': '#004d55',
      'ansi-fg-97': '#000000',
    },
    code: {
      'code-block': '#f7f7fa',
      'code-inline-bg': '#e8e8f0',
      'terminal-bg': '#ffffff',
    },
  },
};
