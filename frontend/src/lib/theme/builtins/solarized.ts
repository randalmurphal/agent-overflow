// Solarized — Ethan Schoonover's canonical palette, both variants.
//
//   base03 #002b36  base02 #073642  base01 #586e75  base00 #657b83
//   base0  #839496  base1  #93a1a1  base2  #eee8d5  base3  #fdf6e3
//   yellow #b58900  orange #cb4b16  red    #dc322f  magenta #d33682
//   violet #6c71c4  blue   #268bd2  cyan   #2aa198  green   #859900
//
// Solarized is the one palette here designed as a light/dark PAIR: the eight
// accents are meant to hold on either ground, and only the base tones swap.
// So both variants ship, and a mode flip keeps the accents in place.
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
//   - the light island's plain-foreground ANSI slot (37) takes base01 rather
//     than base00: Solarized light's body text sits at 4.1:1, half a step
//     under AA, and terminal output is the one place we do not get to inherit
//     that. base01 is the palette's own emphasized-text tone.
//
// The dark variant's comment keeps canonical base01 and is pinned as a
// documented exception in `builtins.contrast.test.ts`.

import type { BuiltinThemeSpec } from '../builtins';

export const SOLARIZED: BuiltinThemeSpec = {
  id: 'solarized',
  name: 'Solarized',
  axes: { ui: false, code: true },
  dark: {
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
      'code-block': '#002b36',
      'code-inline-bg': '#073642',
      'terminal-bg': '#002b36',
    },
  },
  light: {
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
      'terminal-bg': '#fdf6e3',
    },
  },
};
