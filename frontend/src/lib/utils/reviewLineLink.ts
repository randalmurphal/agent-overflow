import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import { hexDigest } from './hexDigest';

// "Copy link to line" in a pull request's diff: the address of the line on
// the forge's own diff page. Neither format is in the forge's
// documentation; both are what the forges' pages put in their address bar,
// so a wrong one only fails to scroll.
//
// GitHub: `<pr url>/files#diff-<sha256 hex of the path><L|R><line>`, L
// with a removed line's old number, R with an added or unchanged line's
// new number. A renamed file hashes its new path.
//
// GitLab: `<mr url>/diffs#<sha1 hex of the path>_<old>_<new>`, GitLab's
// line code. An unchanged line has its two numbers; an added line its new
// number and the old side's counter at that point (the number the next
// old line would have), a removed line its old number and the new side's
// counter.

export type LineLinkForge = 'github' | 'gitlab';

/** A line as the forge's anchor names it: its kind and both sides'
 * positions (the counter for the side it is not on). */
export interface LinePosition {
  kind: 'add' | 'del' | 'context';
  oldLine: number;
  newLine: number;
}

/**
 * The position of the row at `lineIndex` (its line in the file's body):
 * its own numbers, and the other side's running counter for an added or
 * removed line, read from the hunk the row is in. Null when no row of the
 * file is that line.
 */
export function linePositionAt(file: ReviewFile, lineIndex: number): LinePosition | null {
  const walker = new RowWalker({ ...file, suppressGaps: true });
  while (walker.next()) {
    if (walker.lineIndex !== lineIndex) continue;
    const type = walker.type;
    if (type !== 'add' && type !== 'del' && type !== 'context') return null;
    // After a row the walker's running numbers are the next line's on
    // each side: the side the row is not on has not moved.
    const next = walker.snapshot();
    if (type === 'add') return { kind: 'add', oldLine: next.oldLine, newLine: walker.newLine };
    if (type === 'del') return { kind: 'del', oldLine: walker.oldLine, newLine: next.newLine };
    return { kind: 'context', oldLine: walker.oldLine, newLine: walker.newLine };
  }
  return null;
}

/** The forge's address of a line of `path` in the PR at `prUrl`. */
export async function lineLink(forge: LineLinkForge, prUrl: string, path: string, at: LinePosition): Promise<string> {
  const base = prUrl.replace(/\/+$/, '');
  if (forge === 'github') {
    const side = at.kind === 'del' ? `L${at.oldLine}` : `R${at.newLine}`;
    return `${base}/files#diff-${await hexDigest('SHA-256', path)}${side}`;
  }
  return `${base}/diffs#${await hexDigest('SHA-1', path)}_${at.oldLine}_${at.newLine}`;
}
