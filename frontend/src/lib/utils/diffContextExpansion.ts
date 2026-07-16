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

// Identity memo mirroring buildPatchDisplayRows' WeakMap: the store's
// `files` derived re-maps every parsed file on each rebuild, and the
// expanded file must keep a stable identity (same lines array) or every
// downstream identity-keyed memo re-runs per rebuild. Keyed by the
// expansion STATE (its `lines` Map identity), not the shared base
// array: parsePatchFilesCached returns one base array per patch text,
// so a base-keyed slot would ping-pong between two panes expanding
// identical content and cross-link their predecessor chains.
const expansionCache = new WeakMap<
  Map<number, string>,
  { version: number; base: PatchLine[]; file: PatchFile }
>();

// Expanded array → the array it superseded (the previous expansion
// version from the SAME state, or the base parsed file on the first
// expansion). Consumers with identity-keyed caches (the diff span
// cache) use this to keep serving a superseded array's values for the
// PatchLine objects both arrays share while the expanded array's own
// results are in flight — without it, every expansion click
// re-renders the whole file bare for a round trip.
const predecessors = new WeakMap<PatchLine[], PatchLine[]>();

/** Superseded EXPANDED arrays kept reachable per live chain. Each
 * link strongly retains its predecessor array, so an uncapped chain
 * would grow one full array per expansion click for as long as the
 * file stays loaded; the fallback almost always resolves in one hop
 * (the array that was on screen when the click happened). */
const MAX_RETAINED_PREDECESSORS = 3;

/** The lines array `lines` was rebuilt from, if it came out of
 * applyContextExpansion. */
export function expansionPredecessor(lines: PatchLine[]): PatchLine[] | undefined {
  return predecessors.get(lines);
}

function truncatePredecessorChain(lines: PatchLine[], base: PatchLine[]): void {
  let tail = lines;
  for (let depth = 0; depth < MAX_RETAINED_PREDECESSORS; depth += 1) {
    const next = predecessors.get(tail);
    if (!next || next === base) return; // already within bounds
    tail = next;
  }
  // Drop everything past the deepest retained expanded array, but keep
  // the chain terminated at the BASE array rather than cutting it off:
  // the parse cache retains base anyway (re-pointing costs nothing),
  // and base is usually the only landed entry during a rapid-click
  // burst — severing it would flash shared lines plain, the exact
  // regression this chain exists to prevent.
  predecessors.set(tail, base);
}

// Fetched context lines get a stable PatchLine identity per
// (expansion state, new-side line number). Every rebuild starts from
// the BASE parsed file, so without this each rebuild would mint fresh
// objects for previously fetched lines — breaking every identity-keyed
// downstream memo and defeating the predecessor fallback for exactly
// the region the user just expanded. Keyed by the state's `lines` Map
// identity (the store mutates it in place; a diff reload replaces the
// whole state, which correctly resets the memo).
const contextLineCache = new WeakMap<Map<number, string>, Map<number, PatchLine>>();

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
  const cached = expansionCache.get(state.lines);
  // Same-state only: a state is per (pane, path) and cleared on
  // reload, so a cached file built from a different base array is a
  // defensive impossibility, not a fallback source.
  const prior = cached && cached.base === file.lines ? cached : undefined;
  if (prior && prior.version === state.version) return prior.file;
  const expanded = applyContextExpansionUncached(file, state);
  if (expanded !== file) {
    predecessors.set(expanded.lines, (prior?.file ?? file).lines);
    truncatePredecessorChain(expanded.lines, file.lines);
  }
  expansionCache.set(state.lines, { version: state.version, base: file.lines, file: expanded });
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
      hunk.body.unshift(contextLine(state, hunk.newStart - 1));
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
      hunk.body.push(contextLine(state, endNew + 1));
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

function contextLine(state: ContextExpansionState, lineNo: number): PatchLine {
  const text = state.lines.get(lineNo)!;
  let memo = contextLineCache.get(state.lines);
  if (!memo) {
    memo = new Map();
    contextLineCache.set(state.lines, memo);
  }
  const cached = memo.get(lineNo);
  if (cached && cached.content === ` ${text}`) return cached;
  const line: PatchLine = { content: ` ${text}`, type: 'context' };
  memo.set(lineNo, line);
  return line;
}

function hunkHeaderSuffix(content: string): string {
  const match = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@(.*)$/.exec(content);
  return match?.[1] ?? '';
}
