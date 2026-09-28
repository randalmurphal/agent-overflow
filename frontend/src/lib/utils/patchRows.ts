import { intralineRanges } from './intralineDiff';
import { stripPatchLinePrefix, type DiffGap, type PatchDisplayRow, type PatchLine } from './patchFiles';
import {
  LINE_ADD,
  LINE_DEL,
  LINE_HUNK,
  LINE_MARKER,
  LINE_META,
  lineTypeOf,
  type PatchBody,
  type ReviewFile,
} from './patchStore';

// Display rows of a review file, computed from its compact body the way
// `buildPatchDisplayRows` computes them from a PatchFile: gap rows for
// the hidden runs between hunks, one row per content, marker and fold
// line, and the same row ids and intraline pairing. The walker reads line
// kinds and hunk starts only, so row counts, line numbers and anchor
// lookups never touch text; `materializeRows` builds row objects for
// the rows on screen.

/** The walk state before a display row. */
export interface RowStart {
  /** The next line of the file to read. */
  line: number;
  /** Index of the next display row. */
  row: number;
  /** Non-gap rows before it (the row id's fallback index). */
  content: number;
  /** Gap rows before it (the next gap id). */
  gap: number;
  oldLine: number;
  newLine: number;
  sawHunk: boolean;
}

export const FILE_START: Readonly<RowStart> = Object.freeze({
  line: 0,
  row: 0,
  content: 0,
  gap: 0,
  oldLine: 0,
  newLine: 0,
  sawHunk: false,
});

export type RowType = 'add' | 'del' | 'context' | 'marker' | 'gap';

/** Whether a file shows hunk gaps: conflict pseudo-files and files with
 * `suppressGaps` never do. */
export function fileGapsOn(file: ReviewFile): boolean {
  return !file.suppressGaps && !file.body.hasMarkers;
}

function trailingGapRows(file: ReviewFile): number {
  const body = file.body;
  if (!fileGapsOn(file) || !body.trailingGap) return 0;
  const total = file.newSideTotal;
  return total === undefined || total >= body.endNew ? 1 : 0;
}

/** The file's display row count (`filePatchDisplayRows(file).length`). */
export function displayRowCount(file: ReviewFile): number {
  const body = file.body;
  if (!fileGapsOn(file)) return body.contentRows;
  return body.contentRows + body.gapLines.length + trailingGapRows(file);
}

function lowerBound(sorted: Int32Array, value: number): number {
  let low = 0;
  let high = sorted.length;
  while (low < high) {
    const mid = (low + high) >> 1;
    if (sorted[mid] < value) low = mid + 1;
    else high = mid;
  }
  return low;
}

/**
 * Walks a file's display rows in order without allocating. After
 * `next()` returns true the fields describe the current row.
 */
export class RowWalker {
  type: RowType = 'context';
  /** The row's line index in the file; -1 for gap rows. */
  lineIndex = -1;
  oldLine = 0;
  newLine = 0;
  row = -1;
  /** Non-gap rows before this one (content rows) or the gap's id. */
  content = 0;
  gapId = 0;
  gapStartNew = 0;
  gapEndNew = 0;
  gapHidden = 0;
  gapLocation: DiffGap['location'] = 'between';

  private readonly body: PatchBody;
  private readonly gapsOn: boolean;
  private readonly newSideTotal: number | undefined;
  private line: number;
  private nextRow: number;
  private nextContent: number;
  private nextGap: number;
  private old: number;
  private neu: number;
  private sawHunk: boolean;
  private gapCursor: number;
  private trailingDone = false;

  constructor(file: ReviewFile, start: Readonly<RowStart> = FILE_START) {
    this.body = file.body;
    this.gapsOn = fileGapsOn(file);
    this.newSideTotal = file.newSideTotal;
    this.line = start.line;
    this.nextRow = start.row;
    this.nextContent = start.content;
    this.nextGap = start.gap;
    this.old = start.oldLine;
    this.neu = start.newLine;
    this.sawHunk = start.sawHunk;
    this.gapCursor = lowerBound(this.body.gapLines, start.line);
  }

  /** The walk state before the next row. */
  snapshot(): RowStart {
    return {
      line: this.line,
      row: this.nextRow,
      content: this.nextContent,
      gap: this.nextGap,
      oldLine: this.old,
      newLine: this.neu,
      sawHunk: this.sawHunk,
    };
  }

  next(): boolean {
    const body = this.body;
    while (this.line < body.lineCount) {
      const index = this.line;
      this.line += 1;
      const kind = body.kind(index);
      if (kind === LINE_META) continue;
      if (kind === LINE_HUNK) {
        const oldStart = body.hunkOldStart(index);
        const newStart = body.hunkNewStart(index);
        const gapLines = body.gapLines;
        const opensGap = this.gapCursor < gapLines.length && gapLines[this.gapCursor] === index;
        if (opensGap) this.gapCursor += 1;
        let emitted = false;
        if (opensGap && this.gapsOn) {
          if (!this.sawHunk) {
            this.setGap(1, newStart - 1, newStart - 1, 'leading');
          } else {
            this.setGap(this.neu, newStart - 1, newStart - this.neu, 'between');
          }
          emitted = true;
        }
        this.sawHunk = true;
        this.old = oldStart;
        this.neu = newStart;
        if (emitted) return true;
        continue;
      }
      this.lineIndex = index;
      this.row = this.nextRow;
      this.nextRow += 1;
      this.content = this.nextContent;
      this.nextContent += 1;
      if (kind === LINE_MARKER) {
        this.type = 'marker';
        this.oldLine = 0;
        this.newLine = 0;
      } else if (kind === LINE_DEL) {
        this.type = 'del';
        this.oldLine = this.old;
        this.newLine = 0;
        this.old += 1;
      } else if (kind === LINE_ADD) {
        this.type = 'add';
        this.oldLine = 0;
        this.newLine = this.neu;
        this.neu += 1;
      } else {
        this.type = 'context';
        this.oldLine = this.old;
        this.newLine = this.neu;
        this.old += 1;
        this.neu += 1;
      }
      return true;
    }
    if (this.trailingDone) return false;
    this.trailingDone = true;
    if (!this.gapsOn || !body.trailingGap) return false;
    const total = this.newSideTotal;
    if (total === undefined) {
      this.setGap(this.neu, -1, -1, 'trailing');
      return true;
    }
    if (total >= this.neu) {
      this.setGap(this.neu, total, total - this.neu + 1, 'trailing');
      return true;
    }
    return false;
  }

  /** Line number the review anchors and comment inserts key the row by. */
  anchorLine(): number {
    if (this.type === 'del') return this.oldLine;
    return this.newLine || this.oldLine;
  }

  /** The row id `buildPatchDisplayRows` gives this row. */
  rowId(): string {
    if (this.type === 'gap') return `gap:${this.gapId}:${this.gapStartNew}`;
    return `${this.row}:${this.oldLine}:${this.newLine}:${this.content}`;
  }

  private setGap(startNew: number, endNew: number, hidden: number, location: DiffGap['location']): void {
    this.type = 'gap';
    this.lineIndex = -1;
    this.oldLine = 0;
    this.newLine = 0;
    this.row = this.nextRow;
    this.nextRow += 1;
    this.gapId = this.nextGap;
    this.nextGap += 1;
    this.gapStartNew = startNew;
    this.gapEndNew = endNew;
    this.gapHidden = hidden;
    this.gapLocation = location;
  }
}

/** The walk state before display row `row`. */
export function rowStartAt(file: ReviewFile, row: number): RowStart {
  const walker = new RowWalker(file);
  let state = walker.snapshot();
  while (state.row < row && walker.next()) state = walker.snapshot();
  return state;
}

/** Split-view row count of `count` display rows from `start` — the length
 * `buildSplitDisplayRows` produces for them. */
export function splitRowCount(file: ReviewFile, start: Readonly<RowStart>, count: number): number {
  const walker = new RowWalker(file, start);
  let rows = 0;
  let dels = 0;
  let adds = 0;
  for (let taken = 0; taken < count && walker.next(); taken += 1) {
    if (walker.type === 'del') {
      if (adds > 0) {
        rows += Math.max(dels, adds);
        dels = 0;
        adds = 0;
      }
      dels += 1;
    } else if (walker.type === 'add') {
      if (dels > 0) adds += 1;
      else rows += 1;
    } else {
      if (dels > 0) rows += Math.max(dels, adds);
      dels = 0;
      adds = 0;
      rows += 1;
    }
  }
  if (dels > 0) rows += Math.max(dels, adds);
  return rows;
}

/**
 * Display row objects for `count` rows from `start`, with intraline
 * ranges. Reads the text of those rows and of their del/add partners.
 */
export function materializeRows(file: ReviewFile, start: Readonly<RowStart>, count: number): PatchDisplayRow[] {
  const body = file.body;
  const gapsOn = fileGapsOn(file);
  const walker = new RowWalker(file, start);
  const rows: PatchDisplayRow[] = [];
  const texts = new Map<number, string>();
  const textOf = (index: number): string => {
    let text = texts.get(index);
    if (text === undefined) {
      text = body.text(index);
      texts.set(index, text);
    }
    return text;
  };
  const partners = new Pairing(body, gapsOn);
  while (rows.length < count && walker.next()) {
    if (walker.type === 'gap') {
      rows.push({
        id: walker.rowId(),
        line: { content: '', type: 'context' },
        oldLine: 0,
        newLine: 0,
        side: 'context',
        gap: {
          id: walker.gapId,
          startNew: walker.gapStartNew,
          endNew: walker.gapEndNew,
          hidden: walker.gapHidden,
          location: walker.gapLocation,
        },
      });
      continue;
    }
    const index = walker.lineIndex;
    const kind = body.kind(index);
    const line: PatchLine = { content: textOf(index), type: lineTypeOf(kind) };
    if (kind === LINE_MARKER) {
      const fold = body.fold(index);
      if (fold) line.fold = fold;
    }
    const row: PatchDisplayRow = {
      id: walker.rowId(),
      line,
      oldLine: walker.oldLine,
      newLine: walker.newLine,
      side: walker.type === 'del' ? 'old' : walker.type === 'add' ? 'new' : 'context',
      lineIndex: index,
    };
    if (kind === LINE_DEL || kind === LINE_ADD) {
      const partner = partners.of(index, kind);
      if (partner >= 0) {
        const delText = kind === LINE_DEL ? line.content : textOf(partner);
        const addText = kind === LINE_ADD ? line.content : textOf(partner);
        const ranges = intralineRanges(
          stripPatchLinePrefix({ content: delText, type: 'del' }),
          stripPatchLinePrefix({ content: addText, type: 'add' }),
        );
        const range = ranges ? (kind === LINE_DEL ? ranges.del : ranges.add) : null;
        if (range && range.end > range.start) row.intraline = range;
      }
    }
    rows.push(row);
  }
  return rows;
}

/**
 * The i-th deleted line of a deletion run pairs with the i-th added line
 * of the addition run right after it (split view's pairing, which the
 * intraline highlight follows). Runs are runs of display rows: a context,
 * marker or gap row ends one, while meta lines and hunk headers that open
 * no gap row do not. Consecutive rows of one run reuse the previous
 * answer, so a block pays for one scan per run.
 */
class Pairing {
  private lastIndex = -2;
  private lastKind = -1;
  private lastPartner = -1;

  constructor(private readonly body: PatchBody, private readonly gapsOn: boolean) {}

  of(index: number, kind: number): number {
    let partner: number;
    if (kind === this.lastKind && this.continuesRun(this.lastIndex, index)) {
      partner = this.lastPartner < 0 ? -1 : this.nextOfKind(this.lastPartner + 1, kind === LINE_DEL ? LINE_ADD : LINE_DEL);
    } else {
      partner = kind === LINE_DEL ? this.addFor(index) : this.delFor(index);
    }
    this.lastIndex = index;
    this.lastKind = kind;
    this.lastPartner = partner;
    return partner;
  }

  private passes(index: number): boolean {
    const kind = this.body.kind(index);
    if (kind === LINE_META) return true;
    if (kind !== LINE_HUNK) return false;
    return !(this.gapsOn && this.opensGap(index));
  }

  private opensGap(index: number): boolean {
    const gapLines = this.body.gapLines;
    const at = lowerBound(gapLines, index);
    return at < gapLines.length && gapLines[at] === index;
  }

  private continuesRun(from: number, to: number): boolean {
    if (from < 0 || to <= from) return false;
    for (let index = from + 1; index < to; index += 1) {
      if (!this.passes(index)) return false;
    }
    return true;
  }

  // The next line of `kind` after `from`, across lines that do not end a
  // run; -1 when the run ends first.
  private nextOfKind(from: number, kind: number): number {
    for (let index = from; index < this.body.lineCount; index += 1) {
      const at = this.body.kind(index);
      if (at === kind) return index;
      if (!this.passes(index)) return -1;
    }
    return -1;
  }

  private addFor(index: number): number {
    const body = this.body;
    let ordinal = 0;
    for (let at = index - 1; at >= 0; at -= 1) {
      const kind = body.kind(at);
      if (kind === LINE_DEL) ordinal += 1;
      else if (!this.passes(at)) break;
    }
    let at = index + 1;
    while (at < body.lineCount && (body.kind(at) === LINE_DEL || this.passes(at))) at += 1;
    if (at >= body.lineCount || body.kind(at) !== LINE_ADD) return -1;
    for (let seen = 0; at < body.lineCount; at += 1) {
      const kind = body.kind(at);
      if (kind === LINE_ADD) {
        if (seen === ordinal) return at;
        seen += 1;
      } else if (!this.passes(at)) {
        return -1;
      }
    }
    return -1;
  }

  private delFor(index: number): number {
    const body = this.body;
    let ordinal = 0;
    let at = index - 1;
    for (; at >= 0; at -= 1) {
      const kind = body.kind(at);
      if (kind === LINE_ADD) ordinal += 1;
      else if (!this.passes(at)) break;
    }
    if (at < 0 || body.kind(at) !== LINE_DEL) return -1;
    const lastDel = at;
    while (at >= 0 && (body.kind(at) === LINE_DEL || this.passes(at))) at -= 1;
    for (let seen = 0, line = at + 1; line <= lastDel; line += 1) {
      if (body.kind(line) !== LINE_DEL) continue;
      if (seen === ordinal) return line;
      seen += 1;
    }
    return -1;
  }
}

/** The side and line numbers a comment anchor names. */
export interface RowAnchor {
  side: 'old' | 'new' | 'context' | 'file';
  oldLine?: number;
  newLine?: number;
}

function walkerMatches(walker: RowWalker, anchor: RowAnchor): boolean {
  if (walker.type === 'gap') return false;
  if (anchor.side === 'old') return walker.type === 'del' && walker.oldLine === anchor.oldLine;
  if (anchor.side === 'new') return walker.type === 'add' && walker.newLine === anchor.newLine;
  if (anchor.side === 'context') return walker.oldLine === anchor.oldLine && walker.newLine === anchor.newLine;
  return false;
}

/** Display row index of the row a comment anchor names, or -1. */
export function anchorRow(file: ReviewFile, anchor: RowAnchor): number {
  if (anchor.side === 'file') return -1;
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (walkerMatches(walker, anchor)) return walker.row;
  }
  return -1;
}

/**
 * The anchor lines of a file's rows, by side, for membership checks of
 * many anchors against one file (PR threads). `context` maps a context
 * row's new line to its old line.
 */
export interface AnchorLines {
  old: Set<number>;
  new: Set<number>;
  context: Map<number, number>;
}

export function anchorLines(file: ReviewFile): AnchorLines {
  const out: AnchorLines = { old: new Set(), new: new Set(), context: new Map() };
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (walker.type === 'del') out.old.add(walker.oldLine);
    else if (walker.type === 'add') out.new.add(walker.newLine);
    else if (walker.type === 'context' || walker.type === 'marker') {
      if (!out.context.has(walker.newLine)) out.context.set(walker.newLine, walker.oldLine);
    }
  }
  return out;
}

export function anchorLinesMatch(lines: AnchorLines, anchor: RowAnchor): boolean {
  if (anchor.side === 'old') return anchor.oldLine !== undefined && lines.old.has(anchor.oldLine);
  if (anchor.side === 'new') return anchor.newLine !== undefined && lines.new.has(anchor.newLine);
  if (anchor.side === 'context') {
    return anchor.newLine !== undefined && lines.context.get(anchor.newLine) === anchor.oldLine;
  }
  return false;
}

/**
 * The display row nearest to `line` on `side` (the row's new line, or
 * its old line when it has none), or -1 when no row carries that side.
 * Line numbers rise within a file, so the search stops once it has
 * passed the target and stopped improving.
 */
export function nearestLineRow(file: ReviewFile, side: 'new' | 'old', line: number): number {
  const walker = new RowWalker(file);
  let best = -1;
  let bestDist = Infinity;
  while (walker.next()) {
    if (walker.type === 'gap') continue;
    const rowSide = walker.newLine > 0 ? 'new' : walker.oldLine > 0 ? 'old' : null;
    if (rowSide !== side) continue;
    const rowLine = side === 'new' ? walker.newLine : walker.oldLine;
    const dist = Math.abs(rowLine - line);
    if (dist < bestDist) {
      best = walker.row;
      bestDist = dist;
    }
    if (dist === 0) return best;
    if (rowLine > line && bestDist < dist) return best;
  }
  return best;
}
