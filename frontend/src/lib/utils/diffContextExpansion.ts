import { parseHunkHeader, type DiffGap, type PatchFile, type PatchLine } from './patchFiles';

// Hunk-gap context expansion: merges fetched new-side source lines
// (GetDiffContextLines) back into a parsed PatchFile so the display-row
// builder re-derives numbering, gaps, and intraline pairing from one
// canonical shape. Expanded lines are unchanged on both sides, so a
// merged run extends the adjacent hunk with plain context lines and
// shifts its header start equally on both sides.

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
 * Globally unique version stamp. The identity cache below is
 * module-global and keyed by the SHARED parsed lines array
 * (parsePatchFilesCached returns one result per patch string), while
 * expansion states are per-pane — a per-pane counter would collide
 * when two panes expand the same patch.
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

// Identity memo mirroring buildPatchDisplayRows' WeakMap: the store's
// `files` derived re-maps every parsed file on each rebuild, and the
// expanded file must keep a stable identity (same lines array) or every
// downstream identity-keyed memo re-runs per rebuild.
const expansionCache = new WeakMap<PatchLine[], { version: number; file: PatchFile }>();

/**
 * A copy of `file` with the expansion's fetched lines merged into its
 * hunks as context rows. Returns `file` itself when there is nothing
 * to apply. Never mutates the input (parsed files are shared).
 */
export function applyContextExpansion(
  file: PatchFile,
  state: ContextExpansionState | undefined,
): PatchFile {
  if (!state || (state.lines.size === 0 && state.eofLine === null)) return file;
  const cached = expansionCache.get(file.lines);
  if (cached && cached.version === state.version) return cached.file;
  const expanded = applyContextExpansionUncached(file, state);
  expansionCache.set(file.lines, { version: state.version, file: expanded });
  return expanded;
}

interface HunkSegment {
  oldStart: number;
  newStart: number;
  /** Header text after the closing `@@` (section heading), preserved. */
  suffix: string;
  body: PatchLine[];
}

function applyContextExpansionUncached(file: PatchFile, state: ContextExpansionState): PatchFile {
  // Conflict pseudo-files never emit gaps, so no expansion state can
  // exist for them; guard anyway — their fold/marker rows don't follow
  // hunk numbering.
  if (file.lines.some((line) => line.fold !== undefined || line.type === 'marker')) return file;

  const preamble: PatchLine[] = [];
  const hunks: HunkSegment[] = [];
  let current: HunkSegment | null = null;
  for (const line of file.lines) {
    if (line.type === 'meta') {
      const header = parseHunkHeader(line.content);
      if (header) {
        current = {
          oldStart: header.oldStart,
          newStart: header.newStart,
          suffix: hunkHeaderSuffix(line.content),
          body: [],
        };
        hunks.push(current);
        continue;
      }
    }
    if (current) current.body.push(line);
    else preamble.push(line);
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
    while (hunk.newStart - 1 > prevEnd && state.lines.has(hunk.newStart - 1)) {
      hunk.body.unshift(contextLine(state.lines.get(hunk.newStart - 1)!));
      hunk.newStart -= 1;
      hunk.oldStart -= 1;
    }
    let endNew = hunk.newStart - 1;
    for (const line of hunk.body) {
      if (line.type === 'add' || line.type === 'context') endNew += 1;
    }
    const next = hunks[index + 1];
    const limit = next ? next.newStart - 1 : (state.eofLine ?? Number.MAX_SAFE_INTEGER);
    while (endNew < limit && state.lines.has(endNew + 1)) {
      hunk.body.push(contextLine(state.lines.get(endNew + 1)!));
      endNew += 1;
    }
    prevEnd = endNew;
  }

  const lines: PatchLine[] = [...preamble];
  for (const hunk of hunks) {
    let oldCount = 0;
    let newCount = 0;
    for (const line of hunk.body) {
      if (line.type === 'del' || line.type === 'context') oldCount += 1;
      if (line.type === 'add' || line.type === 'context') newCount += 1;
    }
    lines.push({
      content: `@@ -${hunk.oldStart},${oldCount} +${hunk.newStart},${newCount} @@${hunk.suffix}`,
      type: 'meta',
    });
    lines.push(...hunk.body);
  }
  const expanded: PatchFile = { ...file, lines };
  if (state.eofLine !== null) expanded.newSideTotal = state.eofLine;
  return expanded;
}

function contextLine(text: string): PatchLine {
  return { content: ` ${text}`, type: 'context' };
}

function hunkHeaderSuffix(content: string): string {
  const match = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@(.*)$/.exec(content);
  return match?.[1] ?? '';
}
