import type { LineTintType } from './diffLineTint';

export interface PatchLine {
  content: string;
  type: LineTintType;
  /** Present on conflict-view fold rows (`utils/conflictFile.ts`): a
   * placeholder for `lines` hidden unchanged lines, expandable by id. */
  fold?: { id: number; lines: number };
}

export interface PatchDisplayRow {
  id: string;
  line: PatchLine;
  oldLine: number;
  newLine: number;
  side: 'old' | 'new' | 'context';
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

export function buildPatchDisplayRows(lines: PatchLine[]): PatchDisplayRow[] {
  const rows: PatchDisplayRow[] = [];
  let oldLine = 0;
  let newLine = 0;
  let fallbackIndex = 0;

  for (const line of lines) {
    if (line.type === 'meta') {
      const hunk = parseHunkHeader(line.content);
      if (hunk) {
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

  return rows;
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

function parseHunkHeader(line: string): { oldStart: number; newStart: number } | null {
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
