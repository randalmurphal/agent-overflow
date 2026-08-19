// Solarized — Ethan Schoonover's canonical palette, both variants.
//
//   base03 #002b36  base02 #073642  base01 #586e75  base00 #657b83
//   base0  #839496  base1  #93a1a1  base2  #eee8d5  base3  #fdf6e3
//   yellow #b58900  orange #cb4b16  red    #dc322f  magenta #d33682
//   violet #6c71c4  blue   #268bd2  cyan   #2aa198  green   #859900
//
// Solarized is the one palette here designed as a light/dark PAIR: the eight
// accents are meant to hold on either ground, and only the base tones swap.
// So both variants ship, and a mode flip keeps the accents in place. It serves
// both axes.
//
// SURFACE LADDER. Solarized publishes exactly TWO grounds per mode (base03 +
// base02 dark, base3 + base2 light) against our four tiers, so the missing
// tiers are documented continuations: the same hue and chroma, stepped by
// the same amount the published pair steps by. The dark ladder ENTERS on a
// continuation below base03 — anchored at base03 the app compressed into a
// ~6:1 luminance range, by far the haziest of the curated set (see the
// luminance-range rule in `docs/specs/theme-system.md` §9.11). The invented
// steps are chrome only — no accent, text tone or syntax color is anything
// but Schoonover's.
//
//   dark   #00212b → base03 #002b36 → base02 #073642 → #0e4653
//   light  base3  #fdf6e3 → base2  #eee8d5 → #e0d8c0 → #d3cab0
//
// TEXT is lifted one published step per tier, because Schoonover's
// base0-on-base03 body pairing is exactly the low-contrast look this app
// cannot carry in a wall of prose. FOCAL text takes base2 dark / base02
// light — the tone every Solarized terminal maps bold/bright text onto
// (our own ANSI 37/97 slots already do) — and body copy (`fg-muted`, stated
// explicitly rather than derived) takes the EMPHASIZED base tone (base1 /
// base01). The subtle/hint tiers keep deriving from focal (a stated tier
// claims 4.5:1 text contrast in `builtins.contrast.test.ts`, and
// de-emphasis tiers recede by design). Every tone is Schoonover's; only the
// assignments move, and by one step each.
//
// ROLE MAPPING follows Schoonover's vim mapping (Statement → green,
// Constant → cyan, Type → yellow, Identifier → blue, Special → red,
// PreProc → orange):
//
//   - `keyword` green, `string`/`number`/`constant` cyan, `type` yellow,
//     `function`/`property`/`tag` blue, `string-special` red.
//   - `variable-builtin` tracks `keyword`, the convention the github default
//     uses (this/self read as language keywords more than as variables).
//   - `namespace` tracks `type`; `attribute` is yellow beside it.
//   - `label` is violet, the one accent no other role claims.
//
// LEGIBILITY DEVIATIONS, all in the LIGHT variant, all minimal darkenings of
// the canonical accent because base3 is a very light ground:
//
//   - green  #859900 → #7f9200 (2.94:1 → 3.20:1 on base3)
//   - cyan   #2aa198 → #26948b (2.90:1 → 3.38:1 on base3)
//   - yellow #b58900 → #a97e00 (2.98:1 → 3.32:1 on base3)
//   - comment base1 #93a1a1 → base00 #657b83. base1 measures 1.9:1 on base3,
//     which is under the hard floor even for a faint-by-design role; base00
//     is the next base tone in, so the change stays inside the palette.
//   - the ACCENT role, in both variants. `--accent-fg` has to be readable on
//     an accent FILL, and Solarized's blue is a mid-tone: #268bd2 tops out at
//     4.08:1 against base03 and 3.37:1 under base3, so neither end of the ramp
//     clears 4.5:1 with it. The fill therefore lightens to #3197de on dark
//     (4.67:1 with base03 as its label) and darkens to #1c6fa8 on light
//     (4.94:1 with base3). Every other use of blue — links, functions, tags —
//     keeps #268bd2.
//   - the light island's plain-foreground ANSI slot (37) takes base01 rather
//     than base00: Solarized light's body text sits at 4.1:1, half a step
//     under AA, and terminal output is the one place we do not get to inherit
//     that. base01 is the palette's own emphasized-text tone.
//
// The dark variant's comment keeps canonical base01, which clears the 3:1
// syntax floor on the #00212b code ground (it needed a documented exception
// back when the code ground was base03).

import type { BuiltinThemeSpec } from '../builtins';

export const SOLARIZED: BuiltinThemeSpec = {
  id: 'solarized',
  name: 'Solarized',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      'surface-0': '#00212b',
      'surface-1': '#002b36',
      'surface-2': '#073642',
      'surface-3': '#0e4653',
      border: '#586e75',
      'border-strong': '#657b83',
      'text-primary': '#eee8d5',
      'text-secondary': '#93a1a1',
      'fg-muted': '#93a1a1',
      accent: '#3197de',
      'accent-fg': '#002b36',
      // Markdown prose roles: this file's markup-* hues (orange headings,
      // blue links, yellow markers), so chat prose and fenced markdown
      // agree when both axes are on Solarized. Bold is green — the hue
      // Schoonover's vim mapping gives Statement, the palette's emphasis
      // accent. Inline-code text is code-axis (see the `code` section
      // below).
      'md-heading': '#cb4b16',
      'md-bold': '#859900',
      'md-link': '#268bd2',
      'md-blockquote': '#839496',
      'md-marker': '#b58900',
      info: '#268bd2',
      success: '#859900',
      error: '#dc322f',
      warning: '#b58900',
      overlay: '#002b36a6',
      'ico-terminal': '#2aa198',
      'ico-file': '#6c71c4',
      'ico-eye': '#268bd2',
      'ico-search': '#268bd2',
      'ico-globe': '#2aa198',
      'ico-robot': '#6c71c4',
      'ico-speech-bubble': '#d33682',
      'ico-checklist': '#859900',
      'ico-puzzle': '#d33682',
      'ico-clock': '#b58900',
      'ico-brain': '#cb4b16',
      'ico-compaction': '#657b83',
    },
    syntax: {
      'syntax-keyword': '#859900',
      'syntax-string': '#2aa198',
      'syntax-string-special': '#dc322f',
      'syntax-comment': '#586e75',
      'syntax-number': '#2aa198',
      'syntax-function': '#268bd2',
      'syntax-type': '#b58900',
      'syntax-variable-builtin': '#859900',
      'syntax-property': '#268bd2',
      'syntax-constant': '#2aa198',
      'syntax-tag': '#268bd2',
      'syntax-attribute': '#b58900',
      'syntax-namespace': '#b58900',
      'syntax-label': '#6c71c4',
      'syntax-markup-heading': '#cb4b16',
      'syntax-markup-link': '#268bd2',
      'syntax-markup-raw': '#2aa198',
      'syntax-markup-list': '#b58900',
      'syntax-markup-quote': '#839496',
      'syntax-added': '#859900',
      'syntax-removed': '#dc322f',
    },
    // Solarized's terminal mapping is famous for routing four bright slots at
    // its base tones (bright green → base01, bright yellow → base00, bright
    // blue → base0, bright cyan → base1); that is kept, because it is what a
    // Solarized terminal looks like. The ONE deviation is bright black, which
    // canonically takes base03 — the ground itself, i.e. invisible on this
    // island — and takes base01 here instead so dim output stays readable.
    ansi: {
      'ansi-fg-30': '#073642',
      'ansi-fg-31': '#dc322f',
      'ansi-fg-32': '#859900',
      'ansi-fg-33': '#b58900',
      'ansi-fg-34': '#268bd2',
      'ansi-fg-35': '#d33682',
      'ansi-fg-36': '#2aa198',
      'ansi-fg-37': '#eee8d5',
      'ansi-fg-90': '#586e75',
      'ansi-fg-91': '#cb4b16',
      'ansi-fg-92': '#586e75',
      'ansi-fg-93': '#657b83',
      'ansi-fg-94': '#839496',
      'ansi-fg-95': '#6c71c4',
      'ansi-fg-96': '#93a1a1',
      'ansi-fg-97': '#fdf6e3',
    },
    code: {
      'code-block': '#00212b',
      'code-inline-bg': '#073642',
      // Inline-code text beside its chip ground (markup-raw cyan).
      'md-inline-code': '#2aa198',
      'terminal-bg': '#00212b',
    },
  },
  light: {
    colors: {
      'surface-0': '#fdf6e3',
      'surface-1': '#eee8d5',
      'surface-2': '#e0d8c0',
      'surface-3': '#d3cab0',
      border: '#93a1a1',
      'border-strong': '#839496',
      'text-primary': '#073642',
      'text-secondary': '#586e75',
      'fg-muted': '#586e75',
      accent: '#1c6fa8',
      'accent-fg': '#fdf6e3',
      // Light counterparts of the dark prose roles. A two-variant theme
      // must state these in BOTH variants or not at all — the md-* defaults
      // are declared once for both modes, so an omission here would leave
      // the dark values painting on base3 (the resolver's mode-invariant
      // warning covers exactly this). Orange headings and yellow markers
      // survive the light ground at their 3:1 floors; green misses the 4:1
      // body floor on base3 (canonical #859900 2.97:1, and even this file's
      // darkened #7f9200 step only reaches 3.24:1), so bold restates the
      // focal base02 and weight carries the emphasis, while links take the
      // darkened accent blue this file already uses (canonical #268bd2 is
      // 3.41:1).
      // Inline-code text is code-axis (see the `code` section below).
      'md-heading': '#cb4b16',
      'md-bold': '#073642',
      'md-link': '#1c6fa8',
      'md-blockquote': '#657b83',
      'md-marker': '#a97e00',
      info: '#268bd2',
      success: '#7f9200',
      error: '#dc322f',
      warning: '#a97e00',
      overlay: '#002b3647',
      'ico-terminal': '#26948b',
      'ico-file': '#6c71c4',
      'ico-eye': '#268bd2',
      'ico-search': '#268bd2',
      'ico-globe': '#26948b',
      'ico-robot': '#6c71c4',
      'ico-speech-bubble': '#d33682',
      'ico-checklist': '#7f9200',
      'ico-puzzle': '#d33682',
      'ico-clock': '#a97e00',
      'ico-brain': '#cb4b16',
      'ico-compaction': '#657b83',
    },
    syntax: {
      'syntax-keyword': '#7f9200',
      'syntax-string': '#26948b',
      'syntax-string-special': '#dc322f',
      'syntax-comment': '#657b83',
      'syntax-number': '#26948b',
      'syntax-function': '#268bd2',
      'syntax-type': '#a97e00',
      'syntax-variable-builtin': '#7f9200',
      'syntax-property': '#268bd2',
      'syntax-constant': '#26948b',
      'syntax-tag': '#268bd2',
      'syntax-attribute': '#a97e00',
      'syntax-namespace': '#a97e00',
      'syntax-label': '#6c71c4',
      'syntax-markup-heading': '#cb4b16',
      'syntax-markup-link': '#268bd2',
      'syntax-markup-raw': '#26948b',
      'syntax-markup-list': '#a97e00',
      'syntax-markup-quote': '#657b83',
      'syntax-added': '#7f9200',
      'syntax-removed': '#dc322f',
    },
    // Mirror of the dark table with the base tones inverted, and with the
    // plain / dim / brightest foreground slots (37, 90, 97) taking DARK base
    // tones. Those three are the ones our renderer paints ordinary output
    // with, so on a light island they have to be the dark end of the ramp,
    // not Solarized's literal white slot.
    ansi: {
      'ansi-fg-30': '#073642',
      'ansi-fg-31': '#dc322f',
      'ansi-fg-32': '#7f9200',
      'ansi-fg-33': '#a97e00',
      'ansi-fg-34': '#268bd2',
      'ansi-fg-35': '#d33682',
      'ansi-fg-36': '#26948b',
      'ansi-fg-37': '#586e75',
      'ansi-fg-90': '#93a1a1',
      'ansi-fg-91': '#cb4b16',
      'ansi-fg-92': '#586e75',
      'ansi-fg-93': '#657b83',
      'ansi-fg-94': '#839496',
      'ansi-fg-95': '#6c71c4',
      'ansi-fg-96': '#586e75',
      'ansi-fg-97': '#002b36',
    },
    code: {
      'code-block': '#fdf6e3',
      'code-inline-bg': '#eee8d5',
      // Inline-code text on the chip: cyan misses the 4:1 chip floor on
      // base2 (canonical #2aa198 2.58:1, darkened #26948b 3.01:1), so the
      // role restates the focal base02; the chip ground and the mono face
      // carry the code-ness.
      'md-inline-code': '#073642',
      'terminal-bg': '#fdf6e3',
    },
  },
};
