import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import type { RowAnchor } from './patchRows';

// The lines of a pull request's whole diff a PR comment can sit on, by
// file: each hunk's two starts and one kind byte per row, nothing per line
// of text. A commit run that ends at the PR's head shows its lines as they
// are at the head, so a new-side line (path, N) of that run is the same
// physical line as new line N of the whole diff read at the same revision,
// and takes a PR comment exactly when N lies inside one of the whole
// diff's hunks (docs/specs/pull-requests.md#commit-ranges). Old-side
// lines of a run name the run's base, which the whole diff does not show.

const CONTEXT = 0;
const ADD = 1;
const DEL = 2;

interface Hunk {
  oldStart: number;
  newStart: number;
  /** New-side lines the hunk spans (context and added rows). */
  newCount: number;
  oldCount: number;
  kinds: Uint8Array;
}

export interface PRLineIndex {
  readonly files: ReadonlyMap<string, readonly Hunk[]>;
}

/** A whole-diff line a run's new line N resolves to: N, and for an
 * unchanged line its old number in the whole diff. */
export interface PRLine {
  newLine: number;
  oldLine?: number;
}

/**
 * Builds the index from the whole diff's files. Reads only line kinds and
 * hunk starts, which stay when patch text is evicted. Expansion rows are
 * not hunk rows: pass the files as loaded.
 */
export function buildPRLineIndex(files: readonly ReviewFile[]): PRLineIndex {
  const out = new Map<string, Hunk[]>();
  for (const file of files) {
    const hunks: Hunk[] = [];
    let kinds: number[] = [];
    let hunk: Omit<Hunk, 'kinds'> | null = null;
    let nextOld = 0;
    let nextNew = 0;
    const close = () => {
      if (hunk && kinds.length > 0) hunks.push({ ...hunk, kinds: Uint8Array.from(kinds) });
      hunk = null;
      kinds = [];
    };
    const walker = new RowWalker({ ...file, suppressGaps: true });
    while (walker.next()) {
      const type = walker.type;
      if (type !== 'add' && type !== 'del' && type !== 'context') continue;
      // A hunk's rows number both sides contiguously; a jump is a new hunk.
      const continues = hunk !== null
        && (type === 'add' ? walker.newLine === nextNew : type === 'del' ? walker.oldLine === nextOld : walker.oldLine === nextOld && walker.newLine === nextNew);
      if (!continues) {
        close();
        hunk = {
          oldStart: type === 'add' ? nextOldFor(walker) : walker.oldLine,
          newStart: type === 'del' ? nextNewFor(walker) : walker.newLine,
          newCount: 0,
          oldCount: 0,
        };
        nextOld = hunk.oldStart;
        nextNew = hunk.newStart;
      }
      const open = hunk!;
      if (type === 'add') {
        kinds.push(ADD);
        open.newCount += 1;
        nextNew += 1;
      } else if (type === 'del') {
        kinds.push(DEL);
        open.oldCount += 1;
        nextOld += 1;
      } else {
        kinds.push(CONTEXT);
        open.newCount += 1;
        open.oldCount += 1;
        nextOld += 1;
        nextNew += 1;
      }
    }
    close();
    out.set(file.path, hunks);
  }
  return { files: out };
}

// An added row's old-side counter, and a deleted row's new-side counter,
// are the walker's running numbers (the next line the other side would
// take), which it keeps on its snapshot.
function nextOldFor(walker: RowWalker): number {
  return walker.snapshot().oldLine;
}

function nextNewFor(walker: RowWalker): number {
  return walker.snapshot().newLine;
}

/** The whole-diff line new line `line` of `path` is, or null when no hunk
 * of the whole diff holds it. */
export function resolvePRNewLine(index: PRLineIndex, path: string, line: number): PRLine | null {
  if (line <= 0) return null;
  for (const hunk of index.files.get(path) ?? []) {
    if (line < hunk.newStart || line >= hunk.newStart + hunk.newCount) continue;
    let oldLine = hunk.oldStart;
    let newLine = hunk.newStart;
    for (const kind of hunk.kinds) {
      if (kind === DEL) {
        oldLine += 1;
        continue;
      }
      if (newLine === line) return kind === ADD ? { newLine } : { newLine, oldLine };
      newLine += 1;
      if (kind === CONTEXT) oldLine += 1;
    }
  }
  return null;
}

/** Whether a whole-diff old line `line` of `path` is a deleted row. */
function hasPRDeletedLine(index: PRLineIndex, path: string, line: number): boolean {
  for (const hunk of index.files.get(path) ?? []) {
    if (line < hunk.oldStart || line >= hunk.oldStart + hunk.oldCount) continue;
    let oldLine = hunk.oldStart;
    for (const kind of hunk.kinds) {
      if (kind === ADD) continue;
      if (oldLine === line) return kind === DEL;
      oldLine += 1;
    }
  }
  return false;
}

/** Whether a PR draft's anchor (in the whole diff's numbers) names a row
 * of the whole diff, as `rowMatchesAnchor` with `loose` would. */
export function prIndexHasAnchor(index: PRLineIndex, path: string, anchor: RowAnchor): boolean {
  const old = anchor.oldLine || 0;
  const neu = anchor.newLine || 0;
  if (anchor.side === 'file') return index.files.has(path);
  if (neu > 0) {
    const line = resolvePRNewLine(index, path, neu);
    if (!line) return false;
    return old > 0 ? line.oldLine === old : true;
  }
  return old > 0 && hasPRDeletedLine(index, path, old);
}
