// Line-level classifier for unified-diff text. Returns a flat array
// of typed lines (no file grouping — for that, use
// `parsePatchFiles`). Vocabulary deliberately matches `PatchLine`'s
// (`add`/`del`/`meta`/`context`) so both parsers feed the same
// `lineTintClass` helper without translation.

import type { LineTintType } from './diffLineTint';

export interface DiffLine {
  type: LineTintType;
  content: string;
}

export function parseDiffLines(diffText: string): DiffLine[] {
  if (!diffText) return [];

  return diffText.split('\n').map((line): DiffLine => {
    if (line.startsWith('@@')) return { type: 'meta', content: line };
    if (line.startsWith('+') && !line.startsWith('+++')) return { type: 'add', content: line };
    if (line.startsWith('-') && !line.startsWith('---')) return { type: 'del', content: line };
    return { type: 'context', content: line };
  });
}
