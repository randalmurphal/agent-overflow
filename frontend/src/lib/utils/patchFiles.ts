import type { LineTintType } from './diffLineTint';
import { intralineRanges, type IntralineRange } from './intralineDiff';

export interface PatchLine {
  content: string;
  type: LineTintType;
  /** Present on conflict-view fold rows (`utils/conflictFile.ts`): a
   * placeholder for `lines` hidden unchanged lines, expandable by id. */
  fold?: { id: number; lines: number };
}

/** One hidden run of unchanged lines between (or around) hunks,
 * expandable GitHub-style. Line coordinates are NEW-side. */
export interface DiffGap {
  /** Per-file ordinal, stable within one display-row build. */
  id: number;
  /** First hidden new-side line. */
  startNew: number;
  /** Last hidden new-side line; -1 when unknown (trailing gap on a
   * file whose length hasn't been learned from an expansion yet). */
  endNew: number;
  /** Hidden line count; -1 when unknown. */
  hidden: number;
  /** Which edges have an adjacent hunk: a leading gap can only expand
   * up (no hunk above it), a trailing gap only down. */
  location: 'leading' | 'between' | 'trailing';
}

export interface PatchDisplayRow {
  id: string;
  line: PatchLine;
  oldLine: number;
  newLine: number;
  side: 'old' | 'new' | 'context';
  /** Intraline changed range (offsets into the prefix-stripped source
   * text) when this add/del row pairs with a counterpart in the
   * adjacent run. Absent when the pair is mostly different. */
  intraline?: IntralineRange;
  /** Present on synthetic gap rows standing in for the unchanged
   * lines hidden between hunks. `line` is an empty context line so
   * non-gap-aware consumers render a blank instead of crashing. */
  gap?: DiffGap;
}

export interface SplitDisplayRow {
  left: PatchDisplayRow | null;
  right: PatchDisplayRow | null;
}

export interface PatchFile {
  path: string;
  kind: string;
  additions: number;
  deletions: number;
  lines: PatchLine[];
  /** Conflict-region count for `kind === 'conflict'` pseudo-files. */
  conflicts?: number;
  /** Structural-conflict badge for `kind === 'conflict'` pseudo-files
   * with no textual regions, e.g. "modify/delete". */
  conflictLabel?: string;
  /** New-side file length, learned from a context-expansion response.
   * Sizes (or retires) the trailing hunk gap. Absent on plain parses. */
  newSideTotal?: number;
  /** Skip hunk-gap rows entirely. Set for historical edit diffs: only
   * their hunks were persisted, so there is no source to expand from. */
  suppressGaps?: boolean;
}

/** Display rows for a PatchFile — the canonical call shape, so every
 * consumer shares one memo entry per file (see buildPatchDisplayRows). */
export function filePatchDisplayRows(file: PatchFile): PatchDisplayRow[] {
  return buildPatchDisplayRows(file.lines, file.newSideTotal, file.suppressGaps === true);
}

export function patchFileRowId(file: Pick<PatchFile, 'path'>, index: number): string {
  return `${index}:${file.path}`;
}

export function parsePatchFiles(patch: string): PatchFile[] {
  if (!patch.trim()) return [];
  const files: PatchFile[] = [];
  let current: PatchFile | null = null;

  function finish() {
    if (current && current.path) {
      files.push(current);
    }
    current = null;
  }

  const lines = patch.split('\n');
  for (let lineIndex = 0; lineIndex < lines.length; lineIndex += 1) {
    const line = lines[lineIndex] ?? '';
    if (lineIndex === lines.length - 1 && line === '') continue;
    if (line.startsWith('diff --git ')) {
      finish();
      const parts = line.split(/\s+/);
      current = {
        path: cleanPath(parts[3] ?? ''),
        kind: 'modified',
        additions: 0,
        deletions: 0,
        lines: [{ content: line, type: 'meta' }],
      };
      continue;
    }
    if (!current) continue;
    if (line.startsWith('new file')) current.kind = 'added';
    if (line.startsWith('deleted file')) current.kind = 'deleted';
    if (line.startsWith('rename from ')) current.kind = 'renamed';
    if (line.startsWith('rename to ')) current.path = cleanPath(line.slice('rename to '.length));
    if (line.startsWith('+++ ')) {
      const next = cleanPath(line.slice(4));
      if (next && next !== '/dev/null') current.path = next;
    }
    // INVARIANT: this +/- accounting is the panel's authoritative line count,
    // and the header badge must match it. Go mirrors this rule in
    // internal/git/status.go (countAddedLines, for the badge's untracked
    // tally) and its test twin countPatchAddsDels. If you change the add/del
    // rule here — the +++/--- header skips especially — update countAddedLines
    // and the badge==panel tests in status_test.go, or the badge will silently
    // diverge from this panel.
    if (line.startsWith('+') && !line.startsWith('+++')) current.additions += 1;
    if (line.startsWith('-') && !line.startsWith('---')) current.deletions += 1;
    current.lines.push({
      content: line,
      type: line.startsWith('+') && !line.startsWith('+++')
        ? 'add'
        : line.startsWith('-') && !line.startsWith('---')
          ? 'del'
          : isPatchMetaLine(line)
            ? 'meta'
            : 'context',
    });
  }
  finish();
  return files;
}

/**
 * Merge same-path patch sections into one PatchFile per path, preserving
 * first-appearance order. A whole-turn edits concatenation contains one
 * section per tool call, so a file edited twice in a turn parses as two
 * PatchFiles with the same path — but the review surface keys file-header
 * rows, the file tree, and the collapse/comment maps by path, and a
 * duplicate crashes the keyed each. Sections keep their own hunk headers,
 * so the merged file renders them as consecutive hunks in edit order.
 * Line arrays are shared parse-cache state and never mutated; a merged
 * file gets a fresh concatenated array.
 */
export function mergePatchFilesByPath(files: PatchFile[]): PatchFile[] {
  const merged: PatchFile[] = [];
  const indexByPath = new Map<string, number>();
  for (const file of files) {
    const at = indexByPath.get(file.path);
    if (at === undefined) {
      indexByPath.set(file.path, merged.length);
      merged.push(file);
      continue;
    }
    const existing = merged[at];
    merged[at] = {
      ...existing,
      additions: existing.additions + file.additions,
      deletions: existing.deletions + file.deletions,
      // A later section deleting the file is its end state; otherwise the
      // first section's kind (added / renamed) describes the file best.
      kind: file.kind === 'deleted' ? 'deleted' : existing.kind,
      lines: [...existing.lines, ...file.lines],
    };
  }
  return merged;
}

// Content-keyed memo over parsePatchFiles for render-hot preview
// paths: inline diff rows re-parse their preview patch on every windowing
// remount and every item-churn re-derive, and returning the SAME
// parsed array for the same patch string gives downstream
// identity-keyed memos (buildInlineDiffRowsCached's WeakMap) a stable
// key. Callers MUST treat the result as immutable — it is shared
// across rows and remounts.
//
// The cache holds the patch string itself as the key, so it is sized
// for line-bounded preview patches and payload prefixes
// (≤ INLINE_DIFF_PAYLOAD_PREVIEW_BYTES). Inputs too large to share the
// budget bypass the cache; full multi-MB payload parses (review pane,
// revert flow) should keep calling parsePatchFiles.
export const PATCH_PARSE_CACHE_MAX_TOTAL_CHARS = 2 * 1024 * 1024;
export const PATCH_PARSE_CACHE_MAX_ENTRY_CHARS = PATCH_PARSE_CACHE_MAX_TOTAL_CHARS / 4;
const parsePatchCache = new Map<string, PatchFile[]>();
let parsePatchCacheTotalChars = 0;

export function parsePatchFilesCached(patch: string): PatchFile[] {
  if (patch.length > PATCH_PARSE_CACHE_MAX_ENTRY_CHARS) return parsePatchFiles(patch);
  const hit = parsePatchCache.get(patch);
  if (hit) {
    // Refresh recency: Map iterates in insertion order, so re-inserting
    // makes the eviction loop drop the least-recently-USED entry first.
    parsePatchCache.delete(patch);
    parsePatchCache.set(patch, hit);
    return hit;
  }
  const parsed = parsePatchFiles(patch);
  parsePatchCache.set(patch, parsed);
  parsePatchCacheTotalChars += patch.length;
  while (parsePatchCacheTotalChars > PATCH_PARSE_CACHE_MAX_TOTAL_CHARS) {
    const oldest = parsePatchCache.keys().next().value;
    if (oldest === undefined) break;
    parsePatchCache.delete(oldest);
    parsePatchCacheTotalChars -= oldest.length;
  }
  return parsed;
}

/** Test-only reset. The cache is module-global process state; without
 * this, eviction-budget tests would observe entries left by earlier
 * tests in the same file. */
export function __resetParsePatchCacheForTest(): void {
  parsePatchCache.clear();
  parsePatchCacheTotalChars = 0;
}

/** Test-only inspection. */
export function __parsePatchCacheStatsForTest(): { entries: number; chars: number } {
  return { entries: parsePatchCache.size, chars: parsePatchCacheTotalChars };
}

export function extractPatchFile(patch: string, filePath: string): string | null {
  if (!patch.trim() || !filePath) return null;

  let currentBlock: string[] = [];

  function flush(): string | null {
    if (currentBlock.length === 0) return null;
    const block = currentBlock.join('\n');
    const file = parsePatchFiles(block)[0];
    if (file?.path === filePath) return block;
    return null;
  }

  for (const line of patch.split('\n')) {
    if (line.startsWith('diff --git ')) {
      const match = flush();
      if (match !== null) return match;
      currentBlock = [line];
      continue;
    }
    if (currentBlock.length > 0) currentBlock.push(line);
  }

  return flush();
}

// Identity-keyed memo: the review surface derives display rows for
// every file on every row-model rebuild (buildReviewRows AND
// buildInsertsByFile AND anchor checks), always from the same parsed
// `lines` arrays (parsePatchFilesCached returns shared results). One
// build per lines identity also means the intraline pass runs once,
// not per rebuild. `newSideTotal` (learned from a context-expansion
// response) participates in the key: it changes the trailing gap.
const displayRowsCache = new WeakMap<
  PatchLine[],
  { newSideTotal: number | undefined; suppressGaps: boolean; rows: PatchDisplayRow[] }
>();

export function buildPatchDisplayRows(lines: PatchLine[], newSideTotal?: number, suppressGaps = false): PatchDisplayRow[] {
  const cached = displayRowsCache.get(lines);
  if (cached && cached.newSideTotal === newSideTotal && cached.suppressGaps === suppressGaps) return cached.rows;
  const rows = buildPatchDisplayRowsUncached(lines, newSideTotal, suppressGaps);
  attachIntralineRanges(rows);
  displayRowsCache.set(lines, { newSideTotal, suppressGaps, rows });
  return rows;
}

function buildPatchDisplayRowsUncached(lines: PatchLine[], newSideTotal?: number, suppressGaps = false): PatchDisplayRow[] {
  const rows: PatchDisplayRow[] = [];
  let oldLine = 0;
  let newLine = 0;
  let fallbackIndex = 0;
  // Conflict pseudo-files represent hidden runs as their own fold rows
  // (utils/conflictFile.ts) — emitting hunk gaps there would double up.
  const emitGaps = !suppressGaps && !lines.some((line) => line.fold !== undefined || line.type === 'marker');
  let gapId = 0;
  let sawHunk = false;
  // oldStart of the first hunk: 0 marks an added file (fully present,
  // nothing to expand at either end).
  let firstOldStart = 0;
  let lastNewStart = 0;

  function pushGap(startNew: number, endNew: number, hidden: number, location: DiffGap['location']): void {
    rows.push({
      id: `gap:${gapId}:${startNew}`,
      line: { content: '', type: 'context' },
      oldLine: 0,
      newLine: 0,
      side: 'context',
      gap: { id: gapId, startNew, endNew, hidden, location },
    });
    gapId += 1;
  }

  for (const line of lines) {
    if (line.type === 'meta') {
      const hunk = parseHunkHeader(line.content);
      if (hunk) {
        if (emitGaps) {
          if (!sawHunk) {
            firstOldStart = hunk.oldStart;
            // Leading gap: unchanged lines above the first hunk.
            if (hunk.oldStart > 1 && hunk.newStart > 1) {
              pushGap(1, hunk.newStart - 1, hunk.newStart - 1, 'leading');
            }
          } else if (hunk.newStart > 0 && hunk.newStart > newLine) {
            // Between-hunks gap. Old/new hidden counts are equal here
            // (only unchanged lines separate hunks).
            pushGap(newLine, hunk.newStart - 1, hunk.newStart - newLine, 'between');
          }
          sawHunk = true;
          lastNewStart = hunk.newStart;
        }
        oldLine = hunk.oldStart;
        newLine = hunk.newStart;
      }
      continue;
    }

    // Conflict marker/fold rows display (unlike meta) but carry no line
    // numbers and advance neither side — a fold's skipped span is applied
    // by the hunk header that follows it.
    if (line.type === 'marker') {
      rows.push({
        id: `${rows.length}:0:0:${fallbackIndex}`,
        line,
        oldLine: 0,
        newLine: 0,
        side: 'context',
      });
      fallbackIndex += 1;
      continue;
    }

    let rowOldLine = 0;
    let rowNewLine = 0;
    let side: PatchDisplayRow['side'] = 'context';

    if (line.type === 'del') {
      rowOldLine = oldLine;
      oldLine += 1;
      side = 'old';
    } else if (line.type === 'add') {
      rowNewLine = newLine;
      newLine += 1;
      side = 'new';
    } else {
      rowOldLine = oldLine;
      rowNewLine = newLine;
      oldLine += 1;
      newLine += 1;
    }

    rows.push({
      id: `${rows.length}:${rowOldLine}:${rowNewLine}:${fallbackIndex}`,
      line,
      oldLine: rowOldLine,
      newLine: rowNewLine,
      side,
    });
    fallbackIndex += 1;
  }

  // Trailing gap: a modified file usually continues past its last
  // hunk. Skipped for added files (fully present), deleted files (no
  // new side), and once a known total says the last hunk reached EOF.
  if (emitGaps && sawHunk && firstOldStart > 0 && lastNewStart > 0) {
    if (newSideTotal === undefined) {
      pushGap(newLine, -1, -1, 'trailing');
    } else if (newSideTotal >= newLine) {
      pushGap(newLine, newSideTotal, newSideTotal - newLine + 1, 'trailing');
    }
  }

  return rows;
}

/**
 * Pair the i-th deleted line with the i-th added line inside each
 * del-run → add-run block (split view's pairing) and stamp intraline
 * changed ranges onto both rows. Runs once per display-row build.
 */
function attachIntralineRanges(rows: PatchDisplayRow[]): void {
  let index = 0;
  while (index < rows.length) {
    if (rows[index]?.line.type !== 'del') {
      index += 1;
      continue;
    }
    const dels: PatchDisplayRow[] = [];
    while (rows[index]?.line.type === 'del') {
      dels.push(rows[index]);
      index += 1;
    }
    const adds: PatchDisplayRow[] = [];
    while (rows[index]?.line.type === 'add') {
      adds.push(rows[index]);
      index += 1;
    }
    const pairs = Math.min(dels.length, adds.length);
    for (let k = 0; k < pairs; k += 1) {
      const ranges = intralineRanges(
        stripPatchLinePrefix(dels[k].line),
        stripPatchLinePrefix(adds[k].line),
      );
      if (!ranges) continue;
      if (ranges.del.end > ranges.del.start) dels[k].intraline = ranges.del;
      if (ranges.add.end > ranges.add.start) adds[k].intraline = ranges.add;
    }
  }
}

export function buildSplitDisplayRows(rows: PatchDisplayRow[]): SplitDisplayRow[] {
  const splitRows: SplitDisplayRow[] = [];
  let index = 0;

  while (index < rows.length) {
    const row = rows[index];
    if (!row) break;

    if (row.line.type === 'del') {
      const deletions: PatchDisplayRow[] = [];
      while (rows[index]?.line.type === 'del') {
        deletions.push(rows[index]);
        index += 1;
      }

      const additions: PatchDisplayRow[] = [];
      while (rows[index]?.line.type === 'add') {
        additions.push(rows[index]);
        index += 1;
      }

      const rowCount = Math.max(deletions.length, additions.length);
      for (let rowIndex = 0; rowIndex < rowCount; rowIndex += 1) {
        splitRows.push({
          left: deletions[rowIndex] ?? null,
          right: additions[rowIndex] ?? null,
        });
      }
      continue;
    }

    if (row.line.type === 'add') {
      splitRows.push({ left: null, right: row });
      index += 1;
      continue;
    }

    splitRows.push({ left: row, right: row });
    index += 1;
  }

  return splitRows;
}

function cleanPath(raw: string): string {
  return raw.replace(/^"|"$/g, '').replace(/^[ab]\//, '');
}

export function parseHunkHeader(line: string): { oldStart: number; newStart: number } | null {
  const match = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
  if (!match) return null;
  return {
    oldStart: Number(match[1]),
    newStart: Number(match[2]),
  };
}

function isPatchMetaLine(line: string): boolean {
  return line.startsWith('@@')
    || line.startsWith('diff ')
    || line.startsWith('---')
    || line.startsWith('+++')
    || line.startsWith('index ')
    || line.startsWith('new file mode ')
    || line.startsWith('deleted file mode ')
    || line.startsWith('old mode ')
    || line.startsWith('new mode ')
    || line.startsWith('similarity index ')
    || line.startsWith('dissimilarity index ')
    || line.startsWith('rename from ')
    || line.startsWith('rename to ')
    || line.startsWith('copy from ')
    || line.startsWith('copy to ');
}

/**
 * Strip the diff-format `+`/`-` prefix character off an `add`/`del`
 * line so the source-text-only string can be passed to a syntax
 * tokenizer. `meta` and `context` lines pass through unchanged.
 */
export function stripPatchLinePrefix(line: PatchLine): string {
  if (line.type === 'add' || line.type === 'del') return line.content.slice(1);
  return line.content;
}
