import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';

// "Copy link to line" in a pull request's diff. Neither forge documents an
// address for a line of a PR's diff page, so the link is to the file at
// the commit the diff's new side shows. GitHub documents `#L<line>` on a
// file's address
// (docs.github.com, "Creating a permanent link to a code snippet"), so its
// link names the line: `<repo>/blob/<commit>/<path>#L<line>`. GitLab
// documents line permalinks without their address, so its link is the
// file: `<project>/-/blob/<commit>/<path>`. A removed line has no place
// at that commit and gets no link.

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

const REPO_OF_PR: Record<LineLinkForge, RegExp> = {
  github: /^(https?:\/\/[^?#]+?)\/pull\/\d+\/?$/,
  gitlab: /^(https?:\/\/[^?#]+?)\/-\/merge_requests\/\d+\/?$/,
};

/** What the menu item says the link names. */
export function lineLinkLabel(forge: LineLinkForge): string {
  return forge === 'github' ? 'Copy link to line' : 'Copy link to file';
}

/**
 * The forge's address of the line `at` of `path` (its new path) at
 * `commit`, in the repository of the PR at `prUrl`. Null for a removed
 * line. Throws when `prUrl` is not a PR's address on that forge.
 */
export function lineLink(forge: LineLinkForge, prUrl: string, path: string, at: LinePosition, commit: string): string | null {
  if (at.kind === 'del') return null;
  const repo = REPO_OF_PR[forge].exec(prUrl)?.[1];
  if (!repo) throw new Error(`Not a ${forge === 'github' ? 'pull request' : 'merge request'} address: ${prUrl}`);
  if (!/^[0-9a-f]{7,64}$/i.test(commit)) throw new Error(`Not a commit: ${commit}`);
  const file = path.split('/').map(encodeURIComponent).join('/');
  return forge === 'github'
    ? `${repo}/blob/${commit}/${file}#L${at.newLine}`
    : `${repo}/-/blob/${commit}/${file}`;
}
