import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import type { DiffReviewComment } from '../types/models';

interface ExcerptRow {
  lineIndex: number;
  oldLine: number;
  newLine: number;
}

/** The rows around a comment's line, as posted with it. Reads evicted
 * text again first. */
export async function hunkExcerptForComment(
  files: readonly ReviewFile[],
  comment: Pick<DiffReviewComment, 'filePath' | 'oldLine' | 'newLine' | 'side'>,
  context = 3,
): Promise<string> {
  const file = files.find((candidate) => candidate.path === comment.filePath);
  if (!file || comment.side === 'file') return '';
  // Gap rows are UI affordances, not content — an excerpt line for one
  // would render as a blank row in the posted comment.
  const before: ExcerptRow[] = [];
  const excerpt: ExcerptRow[] = [];
  let after = -1;
  const walker = new RowWalker(file);
  while (walker.next() && after !== 0) {
    if (walker.type === 'gap') continue;
    const row = { lineIndex: walker.lineIndex, oldLine: walker.oldLine, newLine: walker.newLine };
    if (after > 0) {
      excerpt.push(row);
      after -= 1;
    } else if (rowMatchesComment(walker, comment)) {
      excerpt.push(...before, row);
      after = context;
    } else {
      before.push(row);
      if (before.length > context) before.shift();
    }
  }
  if (after < 0) return '';
  const first = excerpt[0].lineIndex;
  const last = excerpt[excerpt.length - 1].lineIndex;
  return file.body.whenResident(() => excerpt.map((row) => formatRow(file, row)).join('\n'), first, last + 1);
}

function rowMatchesComment(
  walker: RowWalker,
  comment: Pick<DiffReviewComment, 'oldLine' | 'newLine' | 'side'>,
): boolean {
  if (comment.side === 'old') return walker.type === 'del' && walker.oldLine === comment.oldLine;
  if (comment.side === 'new') return walker.type === 'add' && walker.newLine === comment.newLine;
  return walker.oldLine === comment.oldLine && walker.newLine === comment.newLine;
}

function formatRow(file: ReviewFile, row: ExcerptRow): string {
  const oldLine = row.oldLine > 0 ? String(row.oldLine).padStart(4, ' ') : '    ';
  const newLine = row.newLine > 0 ? String(row.newLine).padStart(4, ' ') : '    ';
  return `${oldLine} ${newLine} ${file.body.text(row.lineIndex)}`;
}
