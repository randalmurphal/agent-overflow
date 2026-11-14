import { namesOneRow, rowMatchesAnchor, RowWalker, type RowType } from './patchRows';
import { keptText, LINE_HUNK, LINE_META, whenResident, type ReviewFile } from './patchStore';
import type { CommentAnchor } from './reviewRows';

// Which line of a reloaded diff an open new-comment editor is on. A line
// is proven only by exact identity: the file version it was proven in
// (git's blob id of that side, or the byte-identical file section when
// the patch names no blob) is the version the new diff shows, so its line
// number on that side names the same line. Nothing is inferred from text
// around it; text only guards against an abbreviated id that collided.
// An editor no diff proves is detached: it keeps its proof, so a later
// diff that shows its version again (a revert, whitespace shown again, a
// rename with unchanged content) places it again, and it renders after a
// suggested row that proves nothing.

/** Why an editor is detached; `file-gone`: its file is not in the diff. */
export type DetachReason = 'line-changed' | 'not-in-diff' | 'file-gone';

/** The UTF-8 bytes a saved comment may quote (MaxDiffReviewSelectedTextBytes). */
export const EDITOR_QUOTE_MAX_BYTES = 8 * 1024;

/** A line's text compared without whitespace, as a hash and a length. */
export interface LineGuard {
  readonly hash: number;
  readonly length: number;
}

export function lineGuard(text: string): LineGuard {
  const normalized = text.replace(/\s+/g, '');
  let hash = 0x811c9dc5;
  for (let index = 0; index < normalized.length; index += 1) {
    hash ^= normalized.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return { hash, length: normalized.length };
}

function sameGuard(a: LineGuard, b: LineGuard): boolean {
  return a.hash === b.hash && a.length === b.length;
}

/**
 * The identity a line was last proven by. `blob`: git's blob ids of the
 * file's two sides and the line's numbers in them (0: none); `context`
 * records that the line was unchanged, which lets either side's id locate
 * it. `section`: the byte-identical file section and the line's index in
 * it. `file`: a file comment, placed while its file is in the diff.
 * `none`: nothing proves it (a row an expansion supplied, a binary or
 * submodule section), so it never follows a reload.
 */
export type EditorProof =
  | { readonly kind: 'none' }
  | { readonly kind: 'file' }
  | {
    readonly kind: 'blob';
    readonly oldBlob: string | null;
    readonly newBlob: string | null;
    readonly oldLine: number;
    readonly newLine: number;
    readonly context: boolean;
  }
  | { readonly kind: 'section'; readonly key: string; readonly lineIndex: number };

const NO_PROOF: EditorProof = { kind: 'none' };

/** What a file's section header proves about its lines. */
export interface FileIdentity {
  readonly oldBlob: string | null;
  readonly newBlob: string | null;
  /** `rename from`: the path the file had before. */
  readonly oldPath: string | null;
  readonly lines: 'blob' | 'section' | 'none';
}

/** What a file's section header proves, from what the parser read of it
 * (ReviewFile.header) and the kinds of its rows: no text, no wait. */
export function fileIdentity(file: ReviewFile): FileIdentity {
  const body = file.body;
  const header = file.header;
  const oldPath = header?.oldPath ?? null;
  let meta = 0;
  while (meta < body.lineCount && body.kind(meta) === LINE_META) meta += 1;
  // Text after the header that does not open a hunk (`Binary files …
  // differ`, a binary patch) has no line numbers to prove.
  const noLines = body.hasMarkers || (meta < body.lineCount && body.kind(meta) !== LINE_HUNK) || header?.submodule === true;
  if (noLines) return { oldBlob: null, newBlob: null, oldPath, lines: 'none' };
  // A section merged from several (an Edits file) or with no index line
  // proves only its own bytes: its first section's ids describe none of
  // the rest.
  if (!header || (file.mergedSections ?? 1) > 1 || header.sections > 1 || !header.indexed || (header.oldBlob === null && header.newBlob === null)) {
    return { oldBlob: null, newBlob: null, oldPath, lines: 'section' };
  }
  return { oldBlob: header.oldBlob, newBlob: header.newBlob, oldPath, lines: 'blob' };
}

async function sectionKey(file: ReviewFile, budget: RelocationBudget): Promise<string | null> {
  budget.spend(file.body.lineCount, file.body.textLength);
  return await file.body.whenResident(() => file.body.contentKey());
}

/** Relocation's shared allowance: lines walked and bytes of text read.
 * Spent past either, the editors not yet proven are detached and the
 * diff shows. It never reads the clock, so placement is the same every
 * run. */
export class RelocationBudget {
  constructor(private rows: number, private bytes: number) {}

  spend(rows: number, bytes = 0): void {
    this.rows -= rows;
    this.bytes -= bytes;
    if (this.rows < 0 || this.bytes < 0) throw new RelocationBudgetSpent();
  }

  /** Spends when it fits; false, spending nothing, when not. */
  trySpend(rows: number, bytes = 0): boolean {
    if (rows > this.rows || bytes > this.bytes) return false;
    this.rows -= rows;
    this.bytes -= bytes;
    return true;
  }
}

/** The default allowance: a few files of tens of thousands of rows. */
export const RELOCATION_BUDGET_ROWS = 400_000;
export const RELOCATION_BUDGET_BYTES = 32 * 1024 * 1024;

/** The largest file whose text the suggestion's text rule reads (rows and
 * bytes); past either it goes straight to the number rule. */
export const SUGGEST_TEXT_MAX_ROWS = 20_000;
export const SUGGEST_TEXT_MAX_BYTES = 2 * 1024 * 1024;

export class RelocationBudgetSpent extends Error {
  constructor() {
    super('relocation budget spent');
  }
}

/** The relocation was abandoned: a newer load, or the pane went. */
export class RelocationCancelled extends Error {
  constructor() {
    super('relocation cancelled');
  }
}

/** One editor to place in a new diff. */
export interface RelocationTarget {
  readonly filePath: string;
  readonly side: CommentAnchor['side'];
  readonly selectedText: string;
  /** Null for a file comment. */
  readonly guard: LineGuard | null;
  /** Placed: its line in the loaded (unexpanded) file on screen. */
  readonly source: { readonly file: ReviewFile; readonly anchor: CommentAnchor } | null;
  /** Detached: what last proved its line. */
  readonly proof: EditorProof;
  /** Its last line number on its side (the new side for `context`), for
   * the suggested row; 0 when unknown. */
  readonly line: number;
}

export interface RelocationOutcome {
  readonly filePath: string;
  /** Its line in the new diff; null when detached. */
  readonly anchor: CommentAnchor | null;
  readonly proof: EditorProof;
  readonly reason?: DetachReason;
  /** Detached in a file of the diff: the row to show it after. */
  readonly suggested: CommentAnchor | null;
}

interface FoundRow {
  type: RowType;
  oldLine: number;
  newLine: number;
  lineIndex: number;
}

/** A row asked for by one coordinate, and the row types that may hold it
 * (or, for a placed editor's own row, its anchor). */
interface RowRequest {
  by: 'old' | 'new' | 'index';
  value: number;
  accept: readonly RowType[];
  anchor?: CommentAnchor;
  /** A row with the coordinate, of an accepted type. */
  row: FoundRow | null;
  /** Whether a row had the coordinate but not an accepted type. */
  mismatched: boolean;
  /** Rows of an accepted type with the coordinate. */
  count: number;
}

const ROW_TYPES: readonly RowType[] = ['add', 'del', 'context'];

/** The add, del and context rows of a file in row order: what one walk
 * of its row metadata records, for the suggested rows. */
interface RowTable {
  length: number;
  type: Uint8Array;
  oldLine: Int32Array;
  newLine: Int32Array;
  lineIndex: Int32Array;
}

function tableRow(table: RowTable, at: number): FoundRow {
  return { type: ROW_TYPES[table.type[at]], oldLine: table.oldLine[at], newLine: table.newLine[at], lineIndex: table.lineIndex[at] };
}

/** One walk of a file's rows answering every request of it and, when
 * `record`, recording its rows. Reads no text. The walk is charged by the
 * file's line count before anything is sized by it, so a file past the
 * allowance allocates no table and is not walked. */
function findRows(file: ReviewFile, requests: readonly RowRequest[], budget: RelocationBudget, record = false): RowTable | null {
  budget.spend(file.body.lineCount);
  const byOld = new Map<number, RowRequest[]>();
  const byNew = new Map<number, RowRequest[]>();
  const byIndex = new Map<number, RowRequest[]>();
  for (const request of requests) {
    const map = request.by === 'old' ? byOld : request.by === 'new' ? byNew : byIndex;
    const list = map.get(request.value);
    if (list) list.push(request);
    else map.set(request.value, [request]);
  }
  let table: RowTable | null = null;
  if (record) {
    // A row is a line of the file: the line count bounds the rows.
    const capacity = file.body.lineCount;
    table = { length: 0, type: new Uint8Array(capacity), oldLine: new Int32Array(capacity), newLine: new Int32Array(capacity), lineIndex: new Int32Array(capacity) };
  }
  const walker = new RowWalker(file);
  while (walker.next()) {
    const type = walker.type;
    if (type === 'gap' || type === 'marker') continue;
    const found: FoundRow = { type, oldLine: walker.oldLine, newLine: walker.newLine, lineIndex: walker.lineIndex };
    if (table && table.length < table.type.length) {
      const at = table.length;
      table.type[at] = ROW_TYPES.indexOf(type);
      table.oldLine[at] = found.oldLine;
      table.newLine[at] = found.newLine;
      table.lineIndex[at] = found.lineIndex;
      table.length += 1;
    }
    const consider = (list: RowRequest[] | undefined): void => {
      for (const request of list ?? []) {
        const accepted = request.anchor
          ? rowMatchesAnchor(type, found.oldLine, found.newLine, request.anchor)
          : request.accept.includes(type);
        if (!accepted) request.mismatched = true;
        else if ((request.count += 1) === 1) request.row = found;
      }
    };
    if (found.oldLine > 0) consider(byOld.get(found.oldLine));
    if (found.newLine > 0) consider(byNew.get(found.newLine));
    consider(byIndex.get(found.lineIndex));
  }
  return table;
}

// The rows an anchor of each side may sit on (patchRows walkerMatches).
const OLD_ROWS: readonly RowType[] = ['del', 'context'];
const NEW_ROWS: readonly RowType[] = ['add', 'context'];
const CONTEXT_ROWS: readonly RowType[] = ['context'];

function sideRows(side: CommentAnchor['side']): readonly RowType[] {
  return side === 'old' ? OLD_ROWS : side === 'new' ? NEW_ROWS : CONTEXT_ROWS;
}

/** The request locating the rows a placed anchor names in its own file,
 * as saving names them (patchRows' rowMatchesAnchor). */
function sourceRequest(anchor: CommentAnchor): RowRequest {
  const neu = anchor.newLine || 0;
  return { by: neu > 0 ? 'new' : 'old', value: neu > 0 ? neu : anchor.oldLine || 0, accept: [], anchor, row: null, mismatched: false, count: 0 };
}

// Git abbreviates ids to a length that grows with the repository: one id
// may be a longer prefix of the same object in another diff.
const MIN_ID_LENGTH = 7;

function sameId(a: string | null, b: string | null): boolean {
  if (a === null || b === null || a.length < MIN_ID_LENGTH || b.length < MIN_ID_LENGTH) return false;
  return a.length <= b.length ? b.startsWith(a) : a.startsWith(b);
}

/**
 * The request that locates a proven line in a file with `identity`, or
 * null when the file is another version on every side that could prove
 * it. The posting side's equal id names the line by that side's number;
 * an unchanged line may also be located through the other side, and is
 * the same line only on a context row there.
 */
function targetRequest(proof: EditorProof, side: CommentAnchor['side'], identity: FileIdentity, key: string | null): RowRequest | null {
  const request = (by: RowRequest['by'], value: number, accept: readonly RowType[]): RowRequest =>
    ({ by, value, accept, row: null, mismatched: false, count: 0 });
  if (proof.kind === 'section') {
    return identity.lines === 'section' && key !== null && key === proof.key ? request('index', proof.lineIndex, sideRows(side)) : null;
  }
  if (proof.kind !== 'blob' || identity.lines !== 'blob') return null;
  const oldSame = sameId(identity.oldBlob, proof.oldBlob) && proof.oldLine > 0;
  const newSame = sameId(identity.newBlob, proof.newBlob) && proof.newLine > 0;
  if (side === 'old') {
    if (oldSame) return request('old', proof.oldLine, OLD_ROWS);
    if (proof.context && newSame) return request('new', proof.newLine, CONTEXT_ROWS);
    return null;
  }
  if (side === 'new') {
    if (newSame) return request('new', proof.newLine, NEW_ROWS);
    if (proof.context && oldSame) return request('old', proof.oldLine, CONTEXT_ROWS);
    return null;
  }
  if (newSame) return request('new', proof.newLine, CONTEXT_ROWS);
  if (oldSame) return request('old', proof.oldLine, CONTEXT_ROWS);
  return null;
}

/** The anchor a row gives an editor of `side`: an unchanged row's both
 * numbers, a deleted row's old one, an added row's new one. */
function anchorAt(filePath: string, side: CommentAnchor['side'], selectedText: string, row: FoundRow): CommentAnchor {
  if (row.type === 'context') return { filePath, side, oldLine: row.oldLine, newLine: row.newLine, selectedText };
  if (side === 'old') return { filePath, side, oldLine: row.oldLine, selectedText };
  return { filePath, side, newLine: row.newLine, selectedText };
}

function rowText(row: Pick<FoundRow, 'type'>, raw: string): string {
  return row.type === 'add' || row.type === 'del' ? raw.slice(1) : raw;
}

/** A row's line number on `side` (the new side for `context`). */
function sideLine(side: CommentAnchor['side'], oldLine: number, newLine: number): number {
  return side === 'old' ? oldLine : newLine;
}

/**
 * The rows detached editors of one file render after, by presentation
 * only, from the rows one walk recorded: the one row on an editor's side
 * whose text equals its text (when `text` can be read), else the first row
 * with its line number on that side, else the nearest one by that number
 * (the earlier on a tie). -1 when the file has no row on its side. One
 * pass over the rows serves every editor; the number rules look up a
 * sorted index.
 */
function suggestRows(table: RowTable, targets: readonly RelocationTarget[], text: ((lineIndex: number) => string) | null): number[] {
  const out = targets.map(() => -1);
  const sides = [...new Set(targets.map((target) => target.side))];
  const accepts = new Map(sides.map((side) => [side, sideRows(side).map((type) => ROW_TYPES.indexOf(type))]));
  // The text rule: every row read once, against every editor's text.
  // Editors of one text and side get one answer, so a row costs one check
  // per side its text is wanted on, however many editors share the text.
  if (text) {
    interface TextGroup { accept: readonly number[]; side: CommentAnchor['side']; at: number; count: number; members: number[] }
    const wanted = new Map<string, TextGroup[]>();
    targets.forEach((target, index) => {
      let groups = wanted.get(target.selectedText);
      if (!groups) {
        groups = [];
        wanted.set(target.selectedText, groups);
      }
      let group = groups.find((one) => one.side === target.side);
      if (!group) {
        group = { accept: accepts.get(target.side)!, side: target.side, at: -1, count: 0, members: [] };
        groups.push(group);
      }
      group.members.push(index);
    });
    for (let at = 0; at < table.length; at += 1) {
      const type = ROW_TYPES[table.type[at]];
      const groups = wanted.get(rowText({ type }, text(table.lineIndex[at])));
      if (!groups) continue;
      for (const group of groups) {
        if (!group.accept.includes(table.type[at])) continue;
        group.count += 1;
        group.at = at;
      }
    }
    for (const groups of wanted.values()) {
      for (const group of groups) {
        if (group.count === 1) for (const index of group.members) out[index] = group.at;
      }
    }
  }
  // The number rules: each side's rows sorted by number, row order kept
  // among equal numbers.
  for (const side of sides) {
    const accept = accepts.get(side)!;
    const rows: number[] = [];
    for (let at = 0; at < table.length; at += 1) if (accept.includes(table.type[at])) rows.push(at);
    const number = (at: number): number => sideLine(side, table.oldLine[at], table.newLine[at]);
    rows.sort((a, b) => number(a) - number(b) || a - b);
    targets.forEach((target, index) => {
      if (target.side !== side || out[index] >= 0 || rows.length === 0) return;
      let low = 0;
      let high = rows.length;
      while (low < high) {
        const mid = (low + high) >> 1;
        if (number(rows[mid]) < target.line) low = mid + 1;
        else high = mid;
      }
      const above = low < rows.length ? rows[low] : -1;
      if (above >= 0 && number(above) === target.line) {
        out[index] = above;
        return;
      }
      // The nearest below: the first row of its number.
      let below = low > 0 ? low - 1 : -1;
      while (below > 0 && number(rows[below - 1]) === number(rows[below])) below -= 1;
      const under = below >= 0 ? rows[below] : -1;
      if (under < 0) out[index] = above;
      else if (above < 0) out[index] = under;
      else {
        const offBelow = target.line - number(under);
        const offAbove = number(above) - target.line;
        out[index] = offBelow < offAbove || (offBelow === offAbove && under < above) ? under : above;
      }
    });
  }
  return out;
}

/** Adds `key` to its file's list in place: one list per file, never copied. */
function groupInto<K>(groups: Map<ReviewFile, K[]>, file: ReviewFile, key: K): void {
  const list = groups.get(file);
  if (list) list.push(key);
  else groups.set(file, [key]);
}

/**
 * Places each target in `files`, a new diff of the same subject. Whether a
 * target's file is in the diff is decided first and spends no budget: a
 * target whose file is not is `file-gone` whatever else happens. Placed
 * targets are then proven in the file they are on, which must still be
 * readable. Every outcome is decided; a spent budget or an unreadable file
 * detaches the targets not yet placed. A detached target in a file of the
 * diff gets a suggested row. Throws RelocationCancelled when `cancelled`
 * turns true across an await.
 */
export async function relocateEditors<K>(
  targets: ReadonlyMap<K, RelocationTarget>,
  files: readonly ReviewFile[],
  budget: RelocationBudget,
  cancelled: () => boolean,
): Promise<Map<K, RelocationOutcome>> {
  const out = new Map<K, RelocationOutcome>();
  const proofs = new Map<K, EditorProof>();
  const identities = new Map<ReviewFile, FileIdentity>();
  const identityOf = (file: ReviewFile): FileIdentity => {
    let identity = identities.get(file);
    if (!identity) {
      identity = fileIdentity(file);
      identities.set(file, identity);
    }
    return identity;
  };
  const check = (): void => {
    if (cancelled()) throw new RelocationCancelled();
  };

  // Each target's file in the new diff, by path or by the path a rename
  // came from, as the parser recorded it: a lookup that reads nothing.
  const byPath = new Map(files.map((file) => [file.path, file]));
  const fileOf = new Map<K, ReviewFile>();
  let renamed: Map<string, ReviewFile> | null = null;
  for (const [key, target] of targets) {
    let file = byPath.get(target.filePath);
    if (!file) {
      if (renamed === null) {
        renamed = new Map();
        for (const candidate of files) {
          const from = candidate.header?.oldPath;
          if (from) renamed.set(from, candidate);
        }
      }
      file = renamed.get(target.filePath);
    }
    if (file) fileOf.set(key, file);
    else out.set(key, { filePath: target.filePath, anchor: null, proof: target.proof, reason: 'file-gone', suggested: null });
  }
  const detachRest = (): void => {
    for (const [key, target] of targets) {
      if (out.has(key)) continue;
      const file = fileOf.get(key)!;
      out.set(key, { filePath: file.path, anchor: null, proof: proofs.get(key) ?? target.proof, reason: 'line-changed', suggested: null });
    }
  };

  try {
    // Prove each placed target in its own file: one walk per file.
    const bySource = new Map<ReviewFile, K[]>();
    for (const [key, target] of targets) {
      if (target.side === 'file') proofs.set(key, { kind: 'file' });
      else if (target.source) groupInto(bySource, target.source.file, key);
      else proofs.set(key, target.proof);
    }
    for (const [file, keys] of bySource) {
      const requests = keys.map((key) => sourceRequest(targets.get(key)!.source!.anchor));
      findRows(file, requests, budget);
      const identity = identityOf(file);
      const key = identity.lines === 'section' ? await sectionKey(file, budget) : null;
      check();
      keys.forEach((targetKey, index) => {
        const row = requests[index].row;
        // No row in the loaded file: an expansion supplied it. A merged
        // file can repeat a coordinate across its sections: which row the
        // anchor named is unknown.
        if (!row || identity.lines === 'none' || !namesOneRow(requests[index].count)) proofs.set(targetKey, NO_PROOF);
        else if (identity.lines === 'section') proofs.set(targetKey, key === null ? NO_PROOF : { kind: 'section', key, lineIndex: row.lineIndex });
        else proofs.set(targetKey, { kind: 'blob', oldBlob: identity.oldBlob, newBlob: identity.newBlob, oldLine: row.oldLine, newLine: row.newLine, context: row.type === 'context' });
      });
    }

    // A target whose file is gone keeps what proves its line, so a later
    // diff with its file places it again.
    for (const [key, outcome] of out) {
      const proof = proofs.get(key);
      if (proof) out.set(key, { ...outcome, proof });
    }

    const byTarget = new Map<ReviewFile, K[]>();
    for (const [key] of targets) {
      const file = fileOf.get(key);
      if (!file) continue;
      const proof = proofs.get(key)!;
      if (proof.kind === 'file') out.set(key, { filePath: file.path, anchor: { filePath: file.path, side: 'file', selectedText: '' }, proof, suggested: null });
      else groupInto(byTarget, file, key);
    }

    // Locate the proven lines: one walk per file, which also records its
    // rows for the suggestions; then the text of the rows found, for the
    // guard.
    for (const [file, keys] of byTarget) {
      const identity = identityOf(file);
      const needsKey = identity.lines === 'section' && keys.some((key) => proofs.get(key)!.kind === 'section');
      const key = needsKey ? await sectionKey(file, budget) : null;
      check();
      const requests = keys.map((targetKey) => targetRequest(proofs.get(targetKey)!, targets.get(targetKey)!.side, identity, key));
      const table = findRows(file, requests.filter((request) => request !== null), budget, true)!;
      const located = requests.flatMap((request) => (request?.row ? [request.row.lineIndex] : []));
      // Spent before the read, by the lengths the rows' metadata gives.
      budget.spend(located.length, located.reduce((bytes, index) => bytes + file.body.lineLength(index), 0));
      const ranges = located.flatMap((index) => file.body.slice(index, index + 1));
      const texts = await whenResident(ranges, () => new Map(located.map((index) => [index, file.body.text(index)])));
      check();
      const detached: K[] = [];
      keys.forEach((targetKey, index) => {
        const target = targets.get(targetKey)!;
        const proof = proofs.get(targetKey)!;
        const request = requests[index];
        const detach = (reason: DetachReason): void => {
          out.set(targetKey, { filePath: file.path, anchor: null, proof, reason, suggested: null });
          detached.push(targetKey);
        };
        if (!request || request.mismatched && !request.row) return detach('line-changed');
        const row = request.row;
        if (!row) return detach('not-in-diff');
        if (!namesOneRow(request.count)) return detach('line-changed');
        if (!target.guard || !sameGuard(target.guard, lineGuard(rowText(row, texts.get(row.lineIndex) ?? '')))) return detach('line-changed');
        const next: EditorProof = proof.kind === 'section'
          ? proof
          : { kind: 'blob', oldBlob: identity.oldBlob, newBlob: identity.newBlob, oldLine: row.oldLine, newLine: row.newLine, context: row.type === 'context' };
        out.set(targetKey, { filePath: file.path, anchor: anchorAt(file.path, target.side, target.selectedText, row), proof: next, suggested: null });
      });
      if (detached.length > 0) await suggest(file, table, detached, targets, out, budget);
      check();
    }
  } catch (err) {
    if (err instanceof RelocationCancelled) throw err;
    if (!(err instanceof RelocationBudgetSpent)) throw err;
  }
  detachRest();
  return out;
}

// Gives the detached targets of `file` their suggested rows. The text rule
// reads the file's text only within the text cap and the budget, so its
// residency wait is bounded; otherwise the number rules decide and only
// the rows suggested are read, for their quote.
async function suggest<K>(
  file: ReviewFile,
  table: RowTable,
  keys: readonly K[],
  targets: ReadonlyMap<K, RelocationTarget>,
  out: Map<K, RelocationOutcome>,
  budget: RelocationBudget,
): Promise<void> {
  const list = keys.map((key) => targets.get(key)!);
  const body = file.body;
  const byText = body.lineCount <= SUGGEST_TEXT_MAX_ROWS && body.textLength <= SUGGEST_TEXT_MAX_BYTES && budget.trySpend(table.length, body.textLength);
  const rows = byText
    ? await body.whenResident(() => suggestRows(table, list, (lineIndex) => body.text(lineIndex)))
    : suggestRows(table, list, null);
  const found = rows.filter((at) => at >= 0);
  budget.spend(found.length, found.reduce((bytes, at) => bytes + body.lineLength(table.lineIndex[at]), 0));
  const ranges = found.flatMap((at) => body.slice(table.lineIndex[at], table.lineIndex[at] + 1));
  const quotes = await whenResident(ranges, () => new Map(found.map((at) => [at, body.text(table.lineIndex[at])])));
  keys.forEach((key, index) => {
    const at = rows[index];
    const outcome = out.get(key)!;
    if (at < 0) return;
    const row = tableRow(table, at);
    // The editor keeps the quote after the chunk it was read from goes.
    out.set(key, { ...outcome, suggested: anchorAt(file.path, targets.get(key)!.side, keptText.own(rowText(row, quotes.get(at) ?? '')), row) });
  });
}
