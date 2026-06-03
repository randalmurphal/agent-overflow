import type { LineTintType } from './diffLineTint';

export interface PatchLine {
  content: string;
  type: LineTintType;
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
