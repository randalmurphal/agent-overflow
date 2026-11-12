import { intralineRanges } from './intralineDiff';
import { stripPatchLinePrefix, type DiffGap, type PatchDisplayRow, type PatchLine } from './patchFiles';
import {
  LINE_ADD,
  LINE_DEL,
  LINE_HUNK,
  LINE_MARKER,
  LINE_META,
  lineTypeOf,
  ownText,
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

export interface MaterializedRows {
  rows: PatchDisplayRow[];
  /** The file's lines [start, end) holding every line the rows read. */
  lines: { start: number; end: number };
  /** Whether every line read was resident. A row over an evicted line is
   * `pending`; a row whose partner's line is evicted lacks its intraline
   * range. */
  complete: boolean;
}

/**
 * Display row objects for `count` rows from `start`, with intraline
 * ranges. Reads the text of those rows and of their del/add partners.
 */
export function materializeRows(file: ReviewFile, start: Readonly<RowStart>, count: number): MaterializedRows {
  const body = file.body;
  const gapsOn = fileGapsOn(file);
  const walker = new RowWalker(file, start);
  const rows: PatchDisplayRow[] = [];
  const lines = { start: start.line, end: start.line };
  let complete = true;
  // Rows outlive the chunks their text was read from (ownText).
  const texts = new Map<number, string | null>();
  const textOf = (index: number): string | null => {
    let text = texts.get(index);
    if (text === undefined) {
      text = body.residentLine(index) ? ownText(body.text(index)) : null;
      texts.set(index, text);
      if (text === null) complete = false;
      lines.start = Math.min(lines.start, index);
      lines.end = Math.max(lines.end, index + 1);
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
    const text = textOf(index);
    const line: PatchLine = { content: text ?? '', type: lineTypeOf(kind) };
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
    if (text === null) row.pending = true;
    if (kind === LINE_DEL || kind === LINE_ADD) {
      // The partner is read even for a placeholder row, so the lines a
      // block needs are known before its text is back.
      const partner = partners.of(index, kind);
      const partnerText = partner >= 0 ? textOf(partner) : null;
      if (text !== null && partnerText !== null) {
        const delText = kind === LINE_DEL ? line.content : partnerText;
        const addText = kind === LINE_ADD ? line.content : partnerText;
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
  return { rows, lines, complete };
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

/**
 * Whether a row of `type` with these numbers is the row `anchor` names.
 * An anchor with both numbers names the unchanged row that has them, from
 * either half of a split row or the unified one: a comment on an unchanged
 * line carries both. With one number, an `old` anchor names a deleted row
 * and a `new` one an added row. A forge thread knows only its side's
 * number: `loose` lets that number name an unchanged row too.
 */
export function rowMatchesAnchor(type: RowType, oldLine: number, newLine: number, anchor: RowAnchor, loose = false): boolean {
  if (type !== 'add' && type !== 'del' && type !== 'context') return false;
  const old = anchor.oldLine || 0;
  const neu = anchor.newLine || 0;
  if (anchor.side === 'file') return false;
  if (old > 0 && neu > 0) return type === 'context' && oldLine === old && newLine === neu;
  if (anchor.side === 'old') return old > 0 && oldLine === old && (type === 'del' || (loose && type === 'context'));
  if (anchor.side === 'new') return neu > 0 && newLine === neu && (type === 'add' || (loose && type === 'context'));
  return false;
}

/** Display row index of the first row a comment anchor names, or -1. */
export function anchorRow(file: ReviewFile, anchor: RowAnchor, loose = false): number {
  if (anchor.side === 'file') return -1;
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (rowMatchesAnchor(walker.type, walker.oldLine, walker.newLine, anchor, loose)) return walker.row;
  }
  return -1;
}

/** The rows of a file an anchor names, from one walk. */
export interface AnchorRows {
  /** How many rows the anchor names. */
  count: number;
  /** The display row of the one at `lineIndex` (the first when none). */
  row: number;
  /** Its place among them, from 0. */
  occurrence: number;
  /** Whether it is an unchanged row. */
  context: boolean;
}

export function anchorRows(file: ReviewFile, anchor: RowAnchor, lineIndex?: number): AnchorRows {
  const out: AnchorRows = { count: 0, row: -1, occurrence: 0, context: false };
  if (anchor.side === 'file') return out;
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (!rowMatchesAnchor(walker.type, walker.oldLine, walker.newLine, anchor)) continue;
    if (out.row < 0 || walker.lineIndex === lineIndex) {
      out.row = walker.row;
      out.occurrence = out.count;
      out.context = walker.type === 'context';
    }
    out.count += 1;
  }
  return out;
}

/**
 * Whether a comment saved with this coordinate can say which row it is
 * on: exactly one row of the file has it. A merged Edits file can repeat
 * a coordinate across its sections. Saving and relocation both use it.
 */
export function namesOneRow(count: number): boolean {
  return count === 1;
}

/**
 * The anchor of the row a one-number anchor reaches on an unchanged line,
 * with both numbers, or the anchor itself: a comment on an unchanged line
 * is saved with both, whichever half it was written on.
 */
export function completeAnchor<T extends RowAnchor>(file: ReviewFile, anchor: T): T {
  if (anchor.side === 'file' || ((anchor.oldLine || 0) > 0 && (anchor.newLine || 0) > 0)) return anchor;
  const walker = new RowWalker(file);
  let context: { oldLine: number; newLine: number } | null = null;
  while (walker.next()) {
    if (!rowMatchesAnchor(walker.type, walker.oldLine, walker.newLine, anchor, true)) continue;
    if (walker.type !== 'context') return anchor;
    context ??= { oldLine: walker.oldLine, newLine: walker.newLine };
  }
  return context ? { ...anchor, ...context } : anchor;
}

/**
 * The anchor lines of a file's rows, for membership checks of many
 * anchors against one file (PR threads): deleted and added rows by their
 * number, unchanged rows by each number to the other.
 */
export interface AnchorLines {
  del: Set<number>;
  add: Set<number>;
  contextByNew: Map<number, number>;
  contextByOld: Map<number, number>;
}

export function anchorLines(file: ReviewFile): AnchorLines {
  const out: AnchorLines = { del: new Set(), add: new Set(), contextByNew: new Map(), contextByOld: new Map() };
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (walker.type === 'del') out.del.add(walker.oldLine);
    else if (walker.type === 'add') out.add.add(walker.newLine);
    else if (walker.type === 'context') {
      if (!out.contextByNew.has(walker.newLine)) out.contextByNew.set(walker.newLine, walker.oldLine);
      if (!out.contextByOld.has(walker.oldLine)) out.contextByOld.set(walker.oldLine, walker.newLine);
    }
  }
  return out;
}

/** Whether `anchor` names a row of the file, as `rowMatchesAnchor` decides. */
export function anchorLinesMatch(lines: AnchorLines, anchor: RowAnchor, loose = false): boolean {
  const old = anchor.oldLine || 0;
  const neu = anchor.newLine || 0;
  if (anchor.side === 'file') return false;
  if (old > 0 && neu > 0) return lines.contextByNew.get(neu) === old || lines.contextByOld.get(old) === neu;
  if (anchor.side === 'old') return old > 0 && (lines.del.has(old) || (loose && lines.contextByOld.has(old)));
  if (anchor.side === 'new') return neu > 0 && (lines.add.has(neu) || (loose && lines.contextByNew.has(neu)));
  return false;
}

/**
 * Rows inserted among a file's rows (comments, editors): under its header,
 * after the row an anchor names, or after its rows when no row is named.
 * A walk of the file's rows takes each row's inserts in row order; an
 * insert follows only a row that is its anchor's own row, never a row of
 * the other side that shares the number. `occurrence` picks one of the
 * rows a repeated coordinate names.
 */
export class InsertSlots<T> {
  private readonly head: T[] = [];
  private readonly byOld = new Map<number, InsertSlot<T>[]>();
  private readonly byNew = new Map<number, InsertSlot<T>[]>();
  private pending = 0;
  private readonly order: T[] = [];

  constructor(private readonly compare: (a: T, b: T) => number) {}

  /** Under the file header. */
  addHead(item: T): void {
    this.head.push(item);
  }

  /** After the row `anchor` names (`occurrence`: which of several). */
  add(item: T, anchor: RowAnchor, loose = false, occurrence = 0): void {
    if (anchor.side === 'file') {
      this.addHead(item);
      return;
    }
    this.order.push(item);
    this.pending += 1;
    const slot: InsertSlot<T> = { item, anchor, loose, occurrence, seen: 0 };
    const neu = anchor.newLine || 0;
    const map = neu > 0 ? this.byNew : this.byOld;
    const key = neu > 0 ? neu : anchor.oldLine || 0;
    const list = map.get(key);
    if (list) list.push(slot);
    else map.set(key, [slot]);
  }

  get size(): number {
    return this.head.length + this.pending;
  }

  /** The inserts under the header, taken. */
  takeHead(): T[] {
    const out = this.head.slice().sort(this.compare);
    this.head.length = 0;
    return out;
  }

  /** The inserts that follow a row with these numbers, taken. */
  take(type: RowType, oldLine: number, newLine: number): T[] | null {
    if (this.pending === 0) return null;
    const out: T[] = [];
    const visit = (map: Map<number, InsertSlot<T>[]>, key: number): void => {
      const list = map.get(key);
      if (!list) return;
      for (let index = 0; index < list.length; index += 1) {
        const slot = list[index];
        if (!rowMatchesAnchor(type, oldLine, newLine, slot.anchor, slot.loose)) continue;
        if (slot.seen < slot.occurrence) {
          slot.seen += 1;
          continue;
        }
        out.push(slot.item);
        list.splice(index, 1);
        index -= 1;
        this.pending -= 1;
      }
      if (list.length === 0) map.delete(key);
    };
    if (newLine > 0) visit(this.byNew, newLine);
    if (oldLine > 0) visit(this.byOld, oldLine);
    return out.length > 0 ? out.sort(this.compare) : null;
  }

  /** The inserts no row took, in the order they were added. */
  takeRest(): T[] {
    if (this.pending === 0) return [];
    const left = new Set<T>();
    for (const map of [this.byNew, this.byOld]) {
      for (const list of map.values()) for (const slot of list) left.add(slot.item);
      map.clear();
    }
    this.pending = 0;
    return this.order.filter((item) => left.has(item));
  }
}

interface InsertSlot<T> {
  item: T;
  anchor: RowAnchor;
  loose: boolean;
  occurrence: number;
  seen: number;
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
    if (rowLine > line && bestDist < dist) break;
  }
  return best;
}
