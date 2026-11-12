// The matches of a find needle in an xterm buffer, by the app's match rule
// (utils/findMatch.ts) applied to each logical line: a row and the rows
// wrapped onto it. Reads only xterm's public buffer API.
//
// A logical line's text is its rows' text as `translateToString` gives it:
// a wide character is one cell's text over two cells, a combining sequence
// one cell's text, an empty cell a space, and the last row trimmed on the
// right. The last cell of a row a wide character wrapped away from is
// padding, not text, so a word broken there still matches.

import type { IBuffer, IBufferCell } from '@xterm/xterm';
import { foldFindText, forEachFindMatch } from '../../utils/findMatch';

/** A match's cells on one row. */
export interface TerminalMatchCells {
  row: number;
  x: number;
  width: number;
}

/** A match: the first row of its logical line, its offset in that line's
 * text, and its cells, one entry per row it covers. */
export interface TerminalMatch {
  line: number;
  offset: number;
  cells: TerminalMatchCells[];
}

// The end column of `row`'s text: short of the padding cell a wide
// character on the next row left.
function rowEnd(buffer: IBuffer, row: number, cell: IBufferCell): number {
  const line = buffer.getLine(row)!;
  const next = buffer.getLine(row + 1);
  if (!next?.isWrapped || line.length === 0) return line.length;
  const padded = next.getCell(0, cell)?.getWidth() === 2 && line.getCell(line.length - 1, cell)?.getChars() === '';
  return padded ? line.length - 1 : line.length;
}

function addCell(match: TerminalMatch, row: number, x: number, width: number): void {
  const last = match.cells.at(-1);
  if (last && last.row === row && last.x + last.width === x) last.width += width;
  else match.cells.push({ row, x, width });
}

// Maps the string offsets of one logical line's matches (ascending) to
// cells, in one walk of its rows.
function cellsOf(
  buffer: IBuffer,
  start: number,
  ends: readonly number[],
  offsets: readonly number[],
  length: number,
  cell: IBufferCell,
): TerminalMatch[] {
  const matches = offsets.map((offset): TerminalMatch => ({ line: start, offset, cells: [] }));
  let first = 0;
  let at = 0;
  for (let r = 0; r < ends.length && first < matches.length; r += 1) {
    const row = start + r;
    const line = buffer.getLine(row)!;
    for (let x = 0; x < ends[r] && first < matches.length;) {
      line.getCell(x, cell);
      const width = cell.getWidth() || 1;
      const size = cell.getChars().length || 1;
      for (let k = first; k < matches.length && matches[k].offset < at + size; k += 1) {
        if (matches[k].offset + length > at) addCell(matches[k], row, x, width);
      }
      at += size;
      x += width;
      while (first < matches.length && matches[first].offset + length <= at) first += 1;
    }
  }
  return matches;
}

/**
 * The matches of `needle` (folded, `findNeedle`) in `buffer`, in buffer
 * order: the newest `limit` of them, scanning up from the bottom.
 */
export function scanTerminalBuffer(buffer: IBuffer, needle: string, limit: number): TerminalMatch[] {
  const found: TerminalMatch[] = [];
  if (!needle) return found;
  const cell = buffer.getNullCell();
  for (let end = buffer.length; end > 0 && found.length < limit;) {
    let start = end - 1;
    while (start > 0 && buffer.getLine(start)?.isWrapped) start -= 1;
    const ends: number[] = [];
    let text = '';
    for (let row = start; row < end; row += 1) {
      const stop = rowEnd(buffer, row, cell);
      ends.push(stop);
      text += buffer.getLine(row)!.translateToString(row === end - 1, 0, stop);
    }
    const offsets: number[] = [];
    forEachFindMatch(foldFindText(text), needle, (at) => offsets.push(at));
    const kept = offsets.slice(Math.max(0, offsets.length - (limit - found.length)));
    const matches = cellsOf(buffer, start, ends, kept, needle.length, cell);
    for (let i = matches.length - 1; i >= 0; i -= 1) found.push(matches[i]);
    end = start;
  }
  return found.reverse();
}
