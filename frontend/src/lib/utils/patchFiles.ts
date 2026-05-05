import type { LineTintType } from './diffLineTint';

export interface PatchLine {
  content: string;
  type: LineTintType;
}

export interface SplitDiffRow {
  left: PatchLine | null;
  right: PatchLine | null;
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

  for (const line of patch.split('\n')) {
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
    if (line.startsWith('+') && !line.startsWith('+++')) current.additions += 1;
    if (line.startsWith('-') && !line.startsWith('---')) current.deletions += 1;
    current.lines.push({
      content: line,
      type: line.startsWith('+') && !line.startsWith('+++')
        ? 'add'
        : line.startsWith('-') && !line.startsWith('---')
          ? 'del'
          : line.startsWith('@@') || line.startsWith('diff ') || line.startsWith('---') || line.startsWith('+++')
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

export function buildSplitRows(lines: PatchLine[]): SplitDiffRow[] {
  const rows: SplitDiffRow[] = [];
  let index = 0;

  while (index < lines.length) {
    const line = lines[index];
    if (!line) break;

    if (line.type === 'del') {
      const deletions: PatchLine[] = [];
      while (lines[index]?.type === 'del') {
        deletions.push(lines[index]);
        index += 1;
      }

      const additions: PatchLine[] = [];
      while (lines[index]?.type === 'add') {
        additions.push(lines[index]);
        index += 1;
      }

      const rowCount = Math.max(deletions.length, additions.length);
      for (let rowIndex = 0; rowIndex < rowCount; rowIndex += 1) {
        rows.push({
          left: deletions[rowIndex] ?? null,
          right: additions[rowIndex] ?? null,
        });
      }
      continue;
    }

    if (line.type === 'add') {
      rows.push({ left: null, right: line });
      index += 1;
      continue;
    }

    rows.push({ left: line, right: line });
    index += 1;
  }

  return rows;
}

function cleanPath(raw: string): string {
  return raw.replace(/^"|"$/g, '').replace(/^[ab]\//, '');
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
