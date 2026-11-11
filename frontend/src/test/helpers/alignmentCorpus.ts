// Markdown with opener- and closer-like text in every context that is not
// a marker: alignment must change nothing about how it parses.
export const NOT_ALIGNMENT_CORPUS = [
  '```\n[center]\nfenced\n[/center]\n```',
  '~~~md\n[right]\ntilde fenced\n[/right]\n~~~',
  '```text\n[center]\n[/cen',
  '    [center]\n    indented code\n    [/center]',
  'Use `[center]` and `[/right]` inline.',
  '[center](https://example.com) and [right](https://example.org) links.',
  '[center]: https://example.com\n\nSee [center].',
  '- [center]\n- item\n- [/center]',
  '1. [right]\n   continued\n   [/right]',
  '- item\n[center]\nlazy',
  '> [center]\n> quoted\n> [/center]',
  '| [center] | [/center] |\n|---|---|\n| [right] | x |',
  '<div>\n[center]\n</div>',
  'Say [center] then [/center] in a sentence.',
  '[center] on the same line\n[/center] too',
  '[center]text\n[/center]x',
  '  [center]\nindented two\n  [/center]',
  'prose\n[center]\nmore prose',
  '[CENTER]\nupper\n[/CENTER]',
  'a closer with no opener\n[/center]\n\n[/right]\n\nafter',
];
