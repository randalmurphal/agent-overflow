// Pure builder for the capped inline diff preview rendered by
// DiffFileBlock. Walks a file's PatchLines, numbers add/del/context
// rows from hunk headers, inserts a separator row between hunks, and
// stops at the preview cap (flagging overflow for the fade-out + the
// "Show full diff in side panel" CTA).
import type { PatchLine } from '../../utils/patchFiles';
import { INLINE_DIFF_PREVIEW_LINE_COUNT } from '../../utils/inlineThreshold';

export type InlineDiffRow =
  | { kind: 'separator' }
  | { kind: 'line'; line: PatchLine; lineNo: number };

export interface InlineDiffRows {
  rows: InlineDiffRow[];
  hasOverflow: boolean;
  maxLineNo: number;
}

// Identity-keyed memo for the default-cap build. `file.lines` identity
// is stable across virtua remounts when the patch was parsed through
// parsePatchFilesCached, so remounting a diff row (or re-deriving its
// presentation on item churn) reuses the built rows instead of
// re-walking the patch. WeakMap keying keeps eviction tied to the
// parsed patch's own lifetime — so, unlike parsePatchFilesCached's
// string-keyed LRU, this needs no size budget or test-reset hook (a
// dropped `lines` array takes its entry with it). Callers MUST treat
// the result as immutable — it is shared across rows and remounts.
const defaultCapRowsCache = new WeakMap<readonly PatchLine[], InlineDiffRows>();

export function buildInlineDiffRowsCached(lines: readonly PatchLine[]): InlineDiffRows {
  const hit = defaultCapRowsCache.get(lines);
  if (hit) return hit;
  const built = buildInlineDiffRows(lines);
  defaultCapRowsCache.set(lines, built);
  return built;
}

export function buildInlineDiffRows(
  lines: readonly PatchLine[],
  cap: number = INLINE_DIFF_PREVIEW_LINE_COUNT,
): InlineDiffRows {
  const rows: InlineDiffRow[] = [];
  let oldNo = 0;
  let newNo = 0;
  let seenFirstHunk = false;
  let hasOverflow = false;
  let maxLineNo = 0;

  function appendRow(row: InlineDiffRow): boolean {
    if (rows.length >= cap) {
      hasOverflow = true;
      return false;
    }
    rows.push(row);
    if (row.kind === 'line' && row.lineNo > maxLineNo) {
      maxLineNo = row.lineNo;
    }
    return true;
  }

  for (const line of lines) {
    if (line.type === 'meta') {
      if (line.content.startsWith('@@')) {
        const parsed = parseHunkHeader(line.content);
        if (parsed) {
          oldNo = parsed.oldStart;
          newNo = parsed.newStart;
        }
        if (seenFirstHunk) {
          if (!appendRow({ kind: 'separator' })) break;
        }
        seenFirstHunk = true;
      }
      continue;
    }
    let lineNo = 0;
    if (line.type === 'add') {
      lineNo = newNo;
      newNo += 1;
    } else if (line.type === 'del') {
      lineNo = oldNo;
      oldNo += 1;
    } else {
      lineNo = newNo;
      oldNo += 1;
      newNo += 1;
    }
    if (!appendRow({ kind: 'line', line, lineNo })) break;
  }
  return { rows, hasOverflow, maxLineNo };
}

function parseHunkHeader(content: string): { oldStart: number; newStart: number } | null {
  const m = content.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
  if (!m || m[1] === undefined || m[2] === undefined) return null;
  return { oldStart: Number(m[1]), newStart: Number(m[2]) };
}
