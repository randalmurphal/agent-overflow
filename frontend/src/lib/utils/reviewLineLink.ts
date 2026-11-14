import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import { sha1Hex, sha256Hex } from './hashes';

// "Copy link to line": the line's place on the PR's own diff page. Neither
// forge documents these anchors; they are what each forge's diff page puts
// in the address bar when a line number is clicked:
//   GitHub  <pr>/files#diff-<sha256 of the path><R|L><n>
//           R<n> is a new-side line (added or unchanged), L<n> a removed one.
//   GitLab  <mr>/diffs#<sha1 of the path>_<old>_<new>
//           An unchanged line has both numbers. An added line pairs its new
//           number with the number the next old line has, a removed line its
//           old number with the number the next new line has.
// A renamed file is hashed by its new path.

export type LineLinkForge = 'github' | 'gitlab';

/** A row's kind and its two sides' numbers. */
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

const PR_URL: Record<LineLinkForge, RegExp> = {
  github: /^https?:\/\/[^?#]+?\/pull\/\d+(?=\/?$)/,
  gitlab: /^https?:\/\/[^?#]+?\/-\/merge_requests\/\d+(?=\/?$)/,
};

/**
 * The address of the line `at` of `path` (the file's new path) on the
 * diff page of the PR at `prUrl`. Throws when `prUrl` is not a PR's
 * address on that forge.
 */
export function lineLink(forge: LineLinkForge, prUrl: string, path: string, at: LinePosition): string {
  const pr = PR_URL[forge].exec(prUrl)?.[0];
  if (!pr) throw new Error(`Not a ${forge === 'github' ? 'pull request' : 'merge request'} address: ${prUrl}`);
  if (forge === 'github') {
    const line = at.kind === 'del' ? `L${at.oldLine}` : `R${at.newLine}`;
    return `${pr}/files#diff-${sha256Hex(path)}${line}`;
  }
  return `${pr}/diffs#${sha1Hex(path)}_${at.oldLine}_${at.newLine}`;
}
