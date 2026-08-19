// Monokai — the classic Wimer Hazenberg palette as it ships in TextMate's
// `Monokai.tmTheme` and every port since. Dark-only.
//
// GROUND. #272822 is the canonical editor background; #3e3d32 is its
// line-highlight shade, which is what the inline-code ground wants.
//
// ROLE MAPPING notes where judgement was involved:
//
//   - `property` stays at the plain foreground (#f8f8f2). Monokai deliberately
//     leaves object properties uncolored, and inventing a hue for them would
//     make the theme stop looking like Monokai. Our github default colors them;
//     this one does not, and that is the palette speaking.
//   - `variable-builtin` is orange (#fd971f), from Monokai's
//     `variable.language` (self/this) rule.
//   - `namespace` tracks `type` (#66d9ef), the same convention the github
//     default uses.
//   - `label` is orange — Monokai's parameter/argument hue, which is the
//     nearest role to a named argument or a jump target.
//   - `string-special` is violet (#ae81ff), from `constant.character.escape`.
//   - `syntax-comment` (#75715e) is canonical and faint; its measured ratio is
//     pinned in `builtins.contrast.test.ts`.

import type { BuiltinThemeSpec } from '../builtins';

export const MONOKAI: BuiltinThemeSpec = {
  id: 'monokai',
  name: 'Monokai',
  axes: { ui: false, code: true },
  dark: {
    syntax: {
      'syntax-keyword': '#f92672',
      'syntax-string': '#e6db74',
      'syntax-string-special': '#ae81ff',
      'syntax-comment': '#75715e',
      'syntax-number': '#ae81ff',
      'syntax-function': '#a6e22e',
      'syntax-type': '#66d9ef',
      'syntax-variable-builtin': '#fd971f',
      'syntax-property': '#f8f8f2',
      'syntax-constant': '#ae81ff',
      'syntax-tag': '#f92672',
      'syntax-attribute': '#a6e22e',
      'syntax-namespace': '#66d9ef',
      'syntax-label': '#fd971f',
      'syntax-markup-heading': '#a6e22e',
      'syntax-markup-link': '#66d9ef',
      'syntax-markup-raw': '#e6db74',
      'syntax-markup-list': '#fd971f',
      'syntax-markup-quote': '#75715e',
      'syntax-added': '#a6e22e',
      'syntax-removed': '#f92672',
    },
    // base16-monokai's ANSI 16. Monokai's bright slots repeat their normal
    // counterparts except for the greys — that is the palette, not an omission.
    ansi: {
      'ansi-fg-30': '#272822',
      'ansi-fg-31': '#f92672',
      'ansi-fg-32': '#a6e22e',
      'ansi-fg-33': '#f4bf75',
      'ansi-fg-34': '#66d9ef',
      'ansi-fg-35': '#ae81ff',
      'ansi-fg-36': '#a1efe4',
      'ansi-fg-37': '#f8f8f2',
      'ansi-fg-90': '#75715e',
      'ansi-fg-91': '#f92672',
      'ansi-fg-92': '#a6e22e',
      'ansi-fg-93': '#f4bf75',
      'ansi-fg-94': '#66d9ef',
      'ansi-fg-95': '#ae81ff',
      'ansi-fg-96': '#a1efe4',
      'ansi-fg-97': '#f9f8f5',
    },
    code: {
      'code-block': '#272822',
      // No inline-code chip pair: the chip is UI-axis prose furniture, and
      // monokai is deliberately code-only (no workbench palette), so inline
      // chips follow whatever UI theme is selected — like bold and links.
      'terminal-bg': '#272822',
    },
  },
};
