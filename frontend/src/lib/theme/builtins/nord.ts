// Nord — the official arctic palette (nordtheme.com, nord0–nord15). Dark-only,
// and a UI theme first: Nord publishes a four-step ground ramp (Polar Night),
// and the surface ladder is that ramp entered one step BELOW it — #242933,
// the darkened-nord0 tone Nord's own site and editor ports use for their
// deepest chrome (no palette number, same standing as #616e88 below). Polar
// Night verbatim compressed the app into an ~11:1 luminance range and read
// hazy beside the default theme (~17:1); see the luminance-range rule in
// `docs/specs/theme-system.md` §9.11.
//
//   Polar Night   nord0 #2e3440  nord1 #3b4252  nord2 #434c5e  nord3 #4c566a
//   Snow Storm    nord4 #d8dee9  nord5 #e5e9f0  nord6 #eceff4
//   Frost         nord7 #8fbcbb  nord8 #88c0d0  nord9 #81a1c1  nord10 #5e81ac
//   Aurora        nord11 #bf616a nord12 #d08770 nord13 #ebcb8b
//                 nord14 #a3be8c nord15 #b48ead
//
// ROLE MAPPING comes from Nord's own syntax guidance, which is unusually
// explicit: nord7 declarations/types/classes, nord8 functions and methods,
// nord9 keywords/operators/tags, nord11 errors and deletions, nord12
// annotations and decorators, nord13 escapes and warnings, nord14 strings and
// insertions, nord15 numbers. Judgement was needed twice:
//
//   - `property` stays at nord4, the plain foreground. Nord leaves object
//     properties uncolored, the same stance Monokai takes.
//   - `label` takes nord12, the annotation/decorator hue — the closest
//     published role to a named argument or a jump target.
//
// LEGIBILITY DEVIATION. Nord's comment tone is nord3 (#4c566a), which measures
// 1.7:1 on nord0; the Nord editor ports already lighten it to #616e88, and
// that still only reaches 2.5:1. Comments here go one step further to #7b88a1
// (3.55:1) so the role clears the floor rather than living on an exception.
// Both canonical values are recorded above so the change is visible.
//
// `border-strong` is #616e88 — the ports' brightened Polar Night step, which
// has no number of its own in the published palette but is Nord's own value.

import type { BuiltinThemeSpec } from '../builtins';

export const NORD: BuiltinThemeSpec = {
  id: 'nord',
  name: 'Nord',
  axes: { ui: true, code: true },
  dark: {
    colors: {
      // Darkened-nord0 ground, then Polar Night nord0–nord2 in order.
      'surface-0': '#242933',
      'surface-1': '#2e3440',
      'surface-2': '#3b4252',
      'surface-3': '#434c5e',
      border: '#4c566a',
      'border-strong': '#616e88',
      'text-primary': '#eceff4',
      'text-secondary': '#d8dee9',
      accent: '#88c0d0',
      'accent-fg': '#242933',
      // Markdown prose roles: this file's markup-* hues (frost for
      // structure, aurora orange for markers), so chat prose and fenced
      // markdown agree when both axes are on Nord. Bold is the aurora
      // yellow (nord13) — the warm hue not already spent on the markers
      // (nord12 carries those). Inline-code text is code-axis (see the
      // `code` section below).
      'md-heading': '#88c0d0',
      'md-bold': '#ebcb8b',
      'md-link': '#81a1c1',
      'md-blockquote': '#7b88a1',
      'md-marker': '#d08770',
      info: '#81a1c1',
      success: '#a3be8c',
      error: '#bf616a',
      warning: '#ebcb8b',
      overlay: '#2e3440a6',
      'ico-terminal': '#8fbcbb',
      'ico-file': '#b48ead',
      'ico-eye': '#88c0d0',
      'ico-search': '#81a1c1',
      'ico-globe': '#5e81ac',
      'ico-robot': '#b48ead',
      'ico-speech-bubble': '#d08770',
      'ico-checklist': '#a3be8c',
      'ico-puzzle': '#8fbcbb',
      'ico-clock': '#ebcb8b',
      'ico-brain': '#bf616a',
      'ico-compaction': '#616e88',
    },
    syntax: {
      'syntax-keyword': '#81a1c1',
      'syntax-string': '#a3be8c',
      'syntax-string-special': '#ebcb8b',
      'syntax-comment': '#7b88a1',
      'syntax-number': '#b48ead',
      'syntax-function': '#88c0d0',
      'syntax-type': '#8fbcbb',
      'syntax-variable-builtin': '#81a1c1',
      'syntax-property': '#d8dee9',
      'syntax-constant': '#b48ead',
      'syntax-tag': '#81a1c1',
      'syntax-attribute': '#8fbcbb',
      'syntax-namespace': '#8fbcbb',
      'syntax-label': '#d08770',
      'syntax-markup-heading': '#88c0d0',
      'syntax-markup-link': '#81a1c1',
      'syntax-markup-raw': '#a3be8c',
      'syntax-markup-list': '#d08770',
      'syntax-markup-quote': '#7b88a1',
      'syntax-added': '#a3be8c',
      'syntax-removed': '#bf616a',
    },
    // Nord's published terminal mapping. Its bright half repeats the Aurora
    // and Frost accents, varying only black, cyan and white.
    ansi: {
      'ansi-fg-30': '#3b4252',
      'ansi-fg-31': '#bf616a',
      'ansi-fg-32': '#a3be8c',
      'ansi-fg-33': '#ebcb8b',
      'ansi-fg-34': '#81a1c1',
      'ansi-fg-35': '#b48ead',
      'ansi-fg-36': '#88c0d0',
      'ansi-fg-37': '#e5e9f0',
      'ansi-fg-90': '#4c566a',
      'ansi-fg-91': '#bf616a',
      'ansi-fg-92': '#a3be8c',
      'ansi-fg-93': '#ebcb8b',
      'ansi-fg-94': '#81a1c1',
      'ansi-fg-95': '#b48ead',
      'ansi-fg-96': '#8fbcbb',
      'ansi-fg-97': '#eceff4',
    },
    code: {
      'code-block': '#242933',
      'code-inline-bg': '#3b4252',
      // Inline-code text beside its chip ground (markup-raw green, nord14).
      'md-inline-code': '#a3be8c',
      'terminal-bg': '#242933',
    },
  },
};
