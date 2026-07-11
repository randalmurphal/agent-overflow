import {
  filePatchDisplayRows,
  type PatchDisplayRow,
  type PatchFile,
} from './patchFiles';
import type { DiffReviewComment } from '../types/models';

export function hunkExcerptForComment(
  files: readonly PatchFile[],
  comment: Pick<DiffReviewComment, 'filePath' | 'oldLine' | 'newLine' | 'side'>,
  context = 3,
): string {
  const file = files.find((candidate) => candidate.path === comment.filePath);
  if (!file) return '';
  // Gap rows are UI affordances, not content — an excerpt line for one
  // would render as a blank row in the posted comment.
  const rows = filePatchDisplayRows(file).filter((row) => !row.gap);
  const index = rows.findIndex((row) => rowMatchesComment(row, comment));
  if (index < 0) return '';
  const start = Math.max(0, index - context);
  const end = Math.min(rows.length, index + context + 1);
  return rows.slice(start, end).map(formatRow).join('\n');
}

function rowMatchesComment(
  row: PatchDisplayRow,
  comment: Pick<DiffReviewComment, 'oldLine' | 'newLine' | 'side'>,
): boolean {
  if (comment.side === 'file') return false;
  if (comment.side === 'old') return row.side === 'old' && row.oldLine === comment.oldLine;
  if (comment.side === 'new') return row.side === 'new' && row.newLine === comment.newLine;
  return row.oldLine === comment.oldLine && row.newLine === comment.newLine;
}

function formatRow(row: PatchDisplayRow): string {
  const oldLine = row.oldLine > 0 ? String(row.oldLine).padStart(4, ' ') : '    ';
  const newLine = row.newLine > 0 ? String(row.newLine).padStart(4, ' ') : '    ';
  return `${oldLine} ${newLine} ${row.line.content}`;
}
