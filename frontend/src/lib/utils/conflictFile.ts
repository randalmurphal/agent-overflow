import type { PatchFile, PatchLine } from './patchFiles';

// Renders a merged blob from `git merge-tree --write-tree` (conflict
// markers included) as a pseudo-diff PatchFile:
//   - "ours" region lines (base branch)  → del rows (red, old-side numbers)
//   - "theirs" region lines (PR head)    → add rows (green, new-side numbers)
//   - marker lines                       → visible unnumbered `marker` rows,
//     relabeled with the friendly base/head labels when provided
//   - non-conflict runs                  → folded to a few context lines
//     around each conflict; the hidden middle becomes an expandable fold row
// Line numbers flow through synthetic `@@` hunk headers (skipped by the
// display-row builder, exactly like real diff meta lines), so the old side
// approximates the base branch's numbering and the new side the head's.

/** Context lines kept visible on each side of a conflict region. */
export const CONFLICT_FOLD_CONTEXT_LINES = 3;
/** Runs whose hidden middle is shorter than this render in full — a fold
 * row replacing a couple of lines saves no space. */
const MIN_FOLD_LINES = 5;

export interface ConflictFileOptions {
  /** Friendly label for the base ("ours") side, e.g. `origin/main`. */
  baseLabel?: string;
  /** Friendly label for the head ("theirs") side, e.g. the PR branch. */
  headLabel?: string;
  /** Fold ids the user has expanded. Ids are allocated in file order and
   * are stable across expansions (every potential fold site consumes an
   * id whether or not it renders folded). */
  expandedFolds?: ReadonlySet<number>;
  /** merge-tree messages attributed to this file (modify/delete,
   * rename/rename, …) — rendered as marker rows at the top of the body.
   * For a file with no textual conflict regions these are the only
   * signal of what went wrong. */
  notes?: readonly string[];
}

type Segment =
  | { kind: 'run'; lines: string[] }
  | {
      kind: 'conflict';
      oursMarker: string;
      ours: string[];
      baseMarker: string | null;
      base: string[];
      theirs: string[];
      theirsMarker: string;
    };

export function conflictPatchFile(
  path: string,
  content: string,
  options: ConflictFileOptions = {},
): PatchFile {
  const lines = content.split('\n');
  if (lines.at(-1) === '') lines.pop();
  const segments = parseSegments(lines);
  const conflictCount = segments.reduce(
    (count, segment) => count + (segment.kind === 'conflict' ? 1 : 0),
    0,
  );

  const out: PatchLine[] = [];
  let additions = 0;
  let deletions = 0;
  let oldLine = 1;
  let newLine = 1;
  // A hunk header is owed before the next numbered row whenever the
  // counters jumped (file start, or a fold that skipped lines).
  let headerPending = true;
  let nextFoldId = 0;

  function flushHeader(): void {
    if (!headerPending) return;
    headerPending = false;
    out.push({ content: `@@ -${oldLine} +${newLine} @@`, type: 'meta' });
  }

  function emitContext(line: string): void {
    flushHeader();
    out.push({ content: ` ${line}`, type: 'context' });
    oldLine += 1;
    newLine += 1;
  }

  function emitOurs(line: string): void {
    flushHeader();
    out.push({ content: `-${line}`, type: 'del' });
    oldLine += 1;
    deletions += 1;
  }

  function emitTheirs(line: string): void {
    flushHeader();
    out.push({ content: `+${line}`, type: 'add' });
    newLine += 1;
    additions += 1;
  }

  function emitMarker(content: string): void {
    out.push({ content, type: 'marker' });
  }

  /** Emit run lines [from, to) either in full (short or expanded) or as
   * one fold row that advances both counters past the hidden lines. */
  function emitFoldable(runLines: string[], from: number, to: number): void {
    const count = to - from;
    if (count <= 0) return;
    const id = nextFoldId;
    nextFoldId += 1;
    if (count < MIN_FOLD_LINES || options.expandedFolds?.has(id)) {
      for (let index = from; index < to; index += 1) emitContext(runLines[index]);
      return;
    }
    out.push({
      content: `⋯ ${count} unchanged lines`,
      type: 'marker',
      fold: { id, lines: count },
    });
    oldLine += count;
    newLine += count;
    headerPending = true;
  }

  const notes = options.notes ?? [];
  for (const note of notes) emitMarker(note);

  if (conflictCount === 0) {
    // No textual markers: the conflict is structural (modify/delete,
    // rename/rename, …) — the note rows above carry the story. Keep the
    // merged content reachable behind one fold instead of dumping it.
    if (notes.length === 0) emitMarker('No conflict markers in the merged result');
    emitFoldable(lines, 0, lines.length);
    return {
      path,
      kind: 'conflict',
      additions,
      deletions,
      lines: out,
      conflicts: 0,
      conflictLabel: conflictLabelFromNotes(notes),
    };
  }

  for (let index = 0; index < segments.length; index += 1) {
    const segment = segments[index];
    if (segment.kind === 'conflict') {
      emitMarker(relabelMarker(segment.oursMarker, '<<<<<<<', options.baseLabel));
      for (const line of segment.ours) emitOurs(line);
      if (segment.baseMarker !== null) {
        emitMarker(segment.baseMarker);
        // Merged-base content (diff3/zdiff3 conflict style) belongs to
        // neither side; render it unnumbered alongside the markers.
        for (const line of segment.base) emitMarker(line);
      }
      emitMarker('=======');
      for (const line of segment.theirs) emitTheirs(line);
      emitMarker(relabelMarker(segment.theirsMarker, '>>>>>>>', options.headLabel));
      continue;
    }

    const followsConflict = index > 0;
    const precedesConflict = index < segments.length - 1;
    const keepHead = followsConflict ? Math.min(CONFLICT_FOLD_CONTEXT_LINES, segment.lines.length) : 0;
    const keepTail = precedesConflict
      ? Math.min(CONFLICT_FOLD_CONTEXT_LINES, segment.lines.length - keepHead)
      : 0;
    for (let lineIndex = 0; lineIndex < keepHead; lineIndex += 1) {
      emitContext(segment.lines[lineIndex]);
    }
    emitFoldable(segment.lines, keepHead, segment.lines.length - keepTail);
    for (let lineIndex = segment.lines.length - keepTail; lineIndex < segment.lines.length; lineIndex += 1) {
      emitContext(segment.lines[lineIndex]);
    }
  }

  return { path, kind: 'conflict', additions, deletions, lines: out, conflicts: conflictCount };
}

/** Badge label for a file whose only conflicts are structural: the type
 * token from git's message, e.g. `CONFLICT (modify/delete): …` →
 * "modify/delete". */
function conflictLabelFromNotes(notes: readonly string[]): string | undefined {
  for (const note of notes) {
    const match = /^CONFLICT \(([^)]+)\)/.exec(note);
    if (match) return match[1];
  }
  return undefined;
}

function relabelMarker(original: string, prefix: string, label: string | undefined): string {
  if (!label) return original;
  return `${prefix} ${label}`;
}

function isMarker(line: string, prefix: string): boolean {
  if (!line.startsWith(prefix)) return false;
  return line.length === prefix.length || line[prefix.length] === ' ';
}

function parseSegments(lines: string[]): Segment[] {
  const segments: Segment[] = [];
  let run: string[] = [];

  function flushRun(): void {
    if (run.length > 0) {
      segments.push({ kind: 'run', lines: run });
      run = [];
    }
  }

  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (isMarker(line, '<<<<<<<')) {
      const conflict = tryParseConflict(lines, index);
      if (conflict) {
        flushRun();
        segments.push(conflict.segment);
        index = conflict.next;
        continue;
      }
    }
    run.push(line);
    index += 1;
  }
  flushRun();
  return segments;
}

/** Parse one conflict region starting at the `<<<<<<<` line. Returns null
 * on a truncated region (no closing `>>>>>>>`), in which case the caller
 * keeps the lines as plain content instead of guessing. */
function tryParseConflict(
  lines: string[],
  start: number,
): { segment: Segment; next: number } | null {
  const oursMarker = lines[start];
  const ours: string[] = [];
  const base: string[] = [];
  const theirs: string[] = [];
  let baseMarker: string | null = null;
  let section: 'ours' | 'base' | 'theirs' = 'ours';

  for (let index = start + 1; index < lines.length; index += 1) {
    const line = lines[index];
    if (section !== 'theirs' && line === '=======') {
      section = 'theirs';
      continue;
    }
    if (section === 'ours' && isMarker(line, '|||||||')) {
      baseMarker = line;
      section = 'base';
      continue;
    }
    if (section === 'theirs' && isMarker(line, '>>>>>>>')) {
      return {
        segment: { kind: 'conflict', oursMarker, ours, baseMarker, base, theirs, theirsMarker: line },
        next: index + 1,
      };
    }
    if (section === 'ours') ours.push(line);
    else if (section === 'base') base.push(line);
    else theirs.push(line);
  }
  return null;
}
