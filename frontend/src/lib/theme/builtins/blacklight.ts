// Blacklight — an original palette, not an upstream port: UV-lamp fluorescence
// on a violet-tinted near-black. Dark-only, both axes.
//
// The idea, for reading the values below: everything sits on a ground one step
// off true black (#000005), and every hue is a fluorescent pop against it —
// magenta #ff6ec7, UV purple #b45cff, cyan #00e5ff, mint #5cffc9, the way
// pigments read under a blacklight. Grew up as a user theme file named
// "neon"; promoted to a built-in 2026-08-20 with its palette unchanged except
// for `syntax-markup-list`, a token added to the registry after the file was
// written (it takes the accent, matching `md-marker`).
//
// SURFACE LADDER is a violet ramp from near-black: #000005 → #0d0d1c →
// #17172e → #222240. The steps are tiny in absolute luminance and clearly
// separable perceptually (1.09–1.14:1 per step); the wide overall range is
// what keeps the fluorescent hues reading as glow rather than haze (the
// luminance-range rule, `docs/architecture/theme-system.md` §9.11).
//
// FOREGROUND TIERS are stated rather than derived — the derived fades over
// #f5f5ff would drift grey, and the stated tiers keep the violet cast all the
// way down to the hint tier (#8a8ac0, 6.5:1 — comfortably above the stated
// floor).
//
// ROLE MAPPING notes where judgement was involved:
//
//   - `md-heading` and `md-marker` are the UV purple accent; `md-bold` is the
//     magenta — bold text is the loudest fluorescent pop in prose.
//   - inline code is the mint-cyan #00ffd5 on its own indigo chip, distinct
//     from the string mint used in fenced code.
//   - `type`/`attribute` share the lighter purple #c77dff, and
//     `namespace`/`string-special` share the cyan — two-role hue shares, on
//     purpose, same convention Dracula ships.
//   - ANSI black (#4a4a6e) is deliberately visible: on a near-black terminal
//     ground the canonical choice (repeat the ground) makes black output
//     unreadable, so it takes the border indigo instead.

import type { BuiltinThemeSpec } from '../builtins';

export const BLACKLIGHT: BuiltinThemeSpec = {
  id: 'blacklight',
  name: 'Blacklight',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#000005',
      'surface-1': '#0d0d1c',
      'surface-2': '#17172e',
      'surface-3': '#222240',
      border: '#3a3a72',
      'border-strong': '#6c6cd8',
      'text-primary': '#f5f5ff',
      'text-secondary': '#b8b8e0',
      'fg-muted': '#e8e8fa',
      'fg-subtle': '#c5c5e8',
      'fg-hint': '#8a8ac0',
      accent: '#b45cff',
      'accent-fg': '#05050a',
      'md-heading': '#b45cff',
      'md-bold': '#ff6ec7',
      'md-link': '#4d9fff',
      'md-blockquote': '#b8b8e0',
      'md-marker': '#b45cff',
      'code-inline-bg': '#1a1a33',
      'md-inline-code': '#00ffd5',
      info: '#00e5ff',
      success: '#3dff8f',
      error: '#ff5c8a',
      warning: '#ffd23d',
      overlay: '#000000b3',
      'ico-terminal': '#00ffd5',
      'ico-file': '#b45cff',
      'ico-eye': '#4d9fff',
      'ico-search': '#00b3ff',
      'ico-globe': '#00e5ff',
      'ico-robot': '#c77dff',
      'ico-speech-bubble': '#ff6ec7',
      'ico-checklist': '#3dff8f',
      'ico-puzzle': '#ff8bfa',
      'ico-clock': '#ffd23d',
      'ico-brain': '#ff5c8a',
      'ico-compaction': '#8a8ab8',
    },
    syntax: {
      'syntax-keyword': '#ff6ec7',
      'syntax-string': '#5cffc9',
      'syntax-string-special': '#00e5ff',
      'syntax-comment': '#8080b0',
      'syntax-number': '#ffd23d',
      'syntax-function': '#4d9fff',
      'syntax-type': '#c77dff',
      'syntax-variable-builtin': '#ff5c8a',
      'syntax-property': '#7fc2ff',
      'syntax-constant': '#ffd23d',
      'syntax-tag': '#ff6ec7',
      'syntax-attribute': '#c77dff',
      'syntax-namespace': '#00e5ff',
      'syntax-label': '#ffd23d',
      'syntax-markup-heading': '#b45cff',
      'syntax-markup-link': '#4d9fff',
      'syntax-markup-raw': '#5cffc9',
      'syntax-markup-list': '#b45cff',
      'syntax-markup-quote': '#8a8ac0',
      'syntax-added': '#3dff8f',
      'syntax-removed': '#ff5c8a',
    },
    ansi: {
      'ansi-fg-30': '#4a4a6e',
      'ansi-fg-31': '#ff5c8a',
      'ansi-fg-32': '#3dff8f',
      'ansi-fg-33': '#ffd23d',
      'ansi-fg-34': '#4d9fff',
      'ansi-fg-35': '#ff6ec7',
      'ansi-fg-36': '#00e5ff',
      'ansi-fg-37': '#f5f5ff',
      'ansi-fg-90': '#8a8ab8',
      'ansi-fg-91': '#ff8fae',
      'ansi-fg-92': '#7dffb0',
      'ansi-fg-93': '#ffe066',
      'ansi-fg-94': '#85bcff',
      'ansi-fg-95': '#ff9ede',
      'ansi-fg-96': '#66eeff',
      'ansi-fg-97': '#ffffff',
    },
    code: {
      // Code lifts ONE half-step off the app ground (#05050f) so a fenced
      // block still reads as a block on the darkest surface in the app.
      'code-block': '#05050f',
      'terminal-bg': '#000005',
    },
  },
};
