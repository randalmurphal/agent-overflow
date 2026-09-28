import {
  formatHunkHeader,
  hunkHeaderSuffix,
  type DiffGap,
} from './patchFiles';
import {
  LINE_ADD,
  LINE_CONTEXT,
  LINE_DEL,
  LINE_HUNK,
  LINE_MARKER,
  PatchBody,
  PatchStore,
  type BodySegment,
  type ReviewFile,
  type StoredLine,
} from './patchStore';

// Hunk-gap context expansion: merges fetched new-side source lines
// (GetDiffContextLines) back into a review file so the row walker
// re-derives numbering, gaps, and intraline pairing from one canonical
// shape. Expanded lines are unchanged on both sides, so a merged run
// extends the adjacent hunk with plain context lines and shifts its
// header start equally on both sides. The expanded file reuses the
// original's lines by range; only rewritten hunk headers and fetched
// lines are new.

/** Lines fetched per expansion click, GitHub-style stepping. */
export const DIFF_CONTEXT_EXPAND_STEP = 20;

export type ExpandDirection = 'up' | 'down' | 'all';

export interface ContextExpansionState {
  /** New-side line number → source text (no diff prefix). */
  lines: Map<number, string>;
  /** New-side file length once an EOF response reveals it; null until
   * known. Sizes (or retires) the file's trailing gap. */
  eofLine: number | null;
  /** Identity-cache stamp; assign from nextExpansionVersion() on every
   * merge. */
  version: number;
}

let versionCounter = 0;

/**
 * Globally unique version stamp for cheap change detection in the
 * per-state identity memo below. Global (not per-pane) so a stamp can
 * never repeat across states.
 */
export function nextExpansionVersion(): number {
  versionCounter += 1;
  return versionCounter;
}

/**
 * The 1-based inclusive new-side range one expansion click fetches.
 * Null when the direction needs a bound the gap doesn't know yet
 * (an unknown-size trailing gap can only step downward).
 */
export function expansionFetchRange(
  gap: DiffGap,
  dir: ExpandDirection,
): { start: number; end: number } | null {
  if (dir === 'all') {
    if (gap.endNew < 0) return null;
    return { start: gap.startNew, end: gap.endNew };
  }
  if (dir === 'down') {
    const stepEnd = gap.startNew + DIFF_CONTEXT_EXPAND_STEP - 1;
    return { start: gap.startNew, end: gap.endNew < 0 ? stepEnd : Math.min(stepEnd, gap.endNew) };
  }
  if (gap.endNew < 0) return null;
  return { start: Math.max(gap.endNew - DIFF_CONTEXT_EXPAND_STEP + 1, gap.startNew), end: gap.endNew };
}

// Identity memo: the store's `files` derived re-maps every parsed file on
// each rebuild, and the expanded file must keep a stable identity or every
// downstream identity-keyed memo re-runs per rebuild. Keyed by the
// expansion STATE (its `lines` Map identity), not the parsed file: two
// panes expanding identical content hold separate states.
const expansionCache = new WeakMap<
  Map<number, string>,
  { version: number; source: ReviewFile; file: ReviewFile }
>();

/**
 * A copy of `file` with the expansion's fetched lines merged into its
 * hunks as context rows. Returns `file` itself when there is nothing
 * to apply. Never mutates the input (parsed files are shared).
 */
export function applyContextExpansion(
  file: ReviewFile,
  state: ContextExpansionState | undefined,
): ReviewFile {
  if (!state || (state.lines.size === 0 && state.eofLine === null)) return file;
  // A hit needs the very file the build came from: a copy that shares
  // its body but not its other fields (the edits scope's suppressGaps)
  // must not be served a build of the original.
  const cached = expansionCache.get(state.lines);
  if (cached && cached.source === file && cached.version === state.version) return cached.file;
  const expanded = applyContextExpansionUncached(file, state);
  expansionCache.set(state.lines, { version: state.version, source: file, file: expanded });
  return expanded;
}

interface Hunk {
  header: number;
  /** One past the hunk's last line. */
  end: number;
  oldStart: number;
  newStart: number;
  suffix: string;
  /** Lines of the original body on each side. */
  oldCount: number;
  newCount: number;
  before: string[];
  after: string[];
}

function applyContextExpansionUncached(file: ReviewFile, state: ContextExpansionState): ReviewFile {
  const body = file.body;
  // Conflict pseudo-files never emit gaps, so no expansion state can
  // exist for them; guard anyway — their fold/marker rows don't follow
  // hunk numbering.
  if (body.hasMarkers) return file;

  const hunks: Hunk[] = [];
  for (let index = 0; index < body.lineCount; index += 1) {
    const kind = body.kind(index);
    if (kind === LINE_HUNK) {
      const previous = hunks[hunks.length - 1];
      if (previous) previous.end = index;
      hunks.push({
        header: index,
        end: body.lineCount,
        oldStart: body.hunkOldStart(index),
        newStart: body.hunkNewStart(index),
        suffix: hunkHeaderSuffix(body.text(index)),
        oldCount: 0,
        newCount: 0,
        before: [],
        after: [],
      });
      continue;
    }
    const hunk = hunks[hunks.length - 1];
    if (!hunk) continue;
    if (kind === LINE_DEL || kind === LINE_CONTEXT) hunk.oldCount += 1;
    if (kind === LINE_ADD || kind === LINE_CONTEXT) hunk.newCount += 1;
    if (kind === LINE_MARKER) return file;
  }
  // No hunks (metadata-only file) or an added file (oldStart 0, fully
  // present): nothing to merge.
  if (hunks.length === 0 || hunks[0].oldStart === 0) return file;

  // Extend each hunk upward (bottom-anchored fetched run) then downward
  // (top-anchored run). Processing in order means a fully fetched gap
  // is consumed by the upper hunk's downward pass first, and the
  // `prevEnd` bound stops the lower hunk's upward pass from re-adding
  // the same lines.
  let prevEnd = 0;
  for (let index = 0; index < hunks.length; index += 1) {
    const hunk = hunks[index];
    const next = hunks[index + 1];
    const nextStart = next?.newStart;
    while (hunk.newStart - 1 > prevEnd && state.lines.has(hunk.newStart - 1)) {
      hunk.before.unshift(state.lines.get(hunk.newStart - 1)!);
      hunk.newStart -= 1;
      hunk.oldStart -= 1;
    }
    let endNew = hunk.newStart - 1 + hunk.before.length + hunk.newCount;
    const limit = nextStart !== undefined ? nextStart - 1 : (state.eofLine ?? Number.MAX_SAFE_INTEGER);
    while (endNew < limit && state.lines.has(endNew + 1)) {
      hunk.after.push(state.lines.get(endNew + 1)!);
      endNew += 1;
    }
    prevEnd = endNew;
  }

  // New lines — each rewritten header and its fetched runs — go in one
  // small store; the original lines are kept by range.
  const added: StoredLine[] = [];
  const placed: { hunk: Hunk; header: number; before: number; after: number }[] = [];
  for (const hunk of hunks) {
    const extra = hunk.before.length + hunk.after.length;
    const header = added.length;
    added.push({
      content: formatHunkHeader(hunk.oldStart, hunk.oldCount + extra, hunk.newStart, hunk.newCount + extra, hunk.suffix),
      kind: LINE_HUNK,
    });
    const before = added.length;
    for (const text of hunk.before) added.push({ content: ` ${text}`, kind: LINE_CONTEXT });
    const after = added.length;
    for (const text of hunk.after) added.push({ content: ` ${text}`, kind: LINE_CONTEXT });
    placed.push({ hunk, header, before, after });
  }
  const side = PatchStore.fromLines(added);
  const segments: BodySegment[] = body.slice(0, hunks[0].header);
  for (const { hunk, header, before, after } of placed) {
    segments.push({ store: side, start: header, end: header + 1 });
    segments.push({ store: side, start: before, end: after });
    segments.push(...body.slice(hunk.header + 1, hunk.end));
    segments.push({ store: side, start: after, end: after + hunk.after.length });
  }
  const expanded: ReviewFile = { ...file, body: new PatchBody(segments) };
  if (state.eofLine !== null) expanded.newSideTotal = state.eofLine;
  return expanded;
}
