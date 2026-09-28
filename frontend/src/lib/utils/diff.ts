// Line-level classifier for unified-diff text. Returns a flat array
// of typed lines (no file grouping — for that, use
// `parsePatchFiles`). Vocabulary deliberately matches `PatchLine`'s
// (`add`/`del`/`meta`/`context`) so both parsers feed the same
// `lineTintClass` helper without translation.

import type { LineTintType } from './diffLineTint';
import { HUNK_ADDED, HUNK_CONTEXT, HUNK_HEADER, HUNK_REMOVED, HunkBody } from './patchFiles';

export interface DiffLine {
  type: LineTintType;
  content: string;
}

export function parseDiffLines(diffText: string): DiffLine[] {
  if (!diffText) return [];

  // A hunk body's lines by its header's counts (a removed `-- note`
  // reads `--- note`), and any other line by its prefix.
  const body = new HunkBody();
  return diffText.split('\n').map((line): DiffLine => {
    switch (body.next(line)) {
      case HUNK_HEADER:
        return { type: 'meta', content: line };
      case HUNK_ADDED:
        return { type: 'add', content: line };
      case HUNK_REMOVED:
        return { type: 'del', content: line };
      case HUNK_CONTEXT:
        return { type: 'context', content: line };
    }
    if (line.startsWith('+') && !line.startsWith('+++')) return { type: 'add', content: line };
    if (line.startsWith('-') && !line.startsWith('---')) return { type: 'del', content: line };
    return { type: 'context', content: line };
  });
}
