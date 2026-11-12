import { RowWalker, type RowType } from './patchRows';
import { LINE_HUNK, LINE_META, whenResident, type BodySegment, type ReviewFile } from './patchStore';
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

const ZERO_ID = /^0+$/;
const INDEX_LINE = /^index ([0-9a-f]+)\.\.([0-9a-f]+)(?: ([0-7]+))?$/;
const MODE_LINE = /^(?:new file|deleted file|old|new) mode ([0-7]+)$/;
const SUBMODULE_MODE = '160000';

/** Reads a file's section header: the text of the metadata lines that
 * lead its body, and nothing after them. */
export async function fileIdentity(file: ReviewFile, budget: RelocationBudget): Promise<FileIdentity> {
  const body = file.body;
  const meta: number[] = [];
  for (let index = 0; index < body.lineCount && body.kind(index) === LINE_META; index += 1) meta.push(index);
  budget.spend(meta.length);
  // Text after the header that does not open a hunk (`Binary files …
  // differ`, a binary patch) has no line numbers to prove.
  let noLines = body.hasMarkers || (meta.length < body.lineCount && body.kind(meta.length) !== LINE_HUNK);
  const ranges: BodySegment[] = meta.flatMap((index) => body.slice(index, index + 1));
  const texts = await whenResident(ranges, () => meta.map((index) => body.text(index)));
  let sections = 0;
  let oldBlob: string | null = null;
  let newBlob: string | null = null;
  let indexed = false;
  let oldPath: string | null = null;
  for (const text of texts) {
    if (text.startsWith('diff --git ')) sections += 1;
    else if (text.startsWith('rename from ')) oldPath = text.slice('rename from '.length);
    const index = INDEX_LINE.exec(text);
    if (index) {
      indexed = true;
      oldBlob = ZERO_ID.test(index[1]) ? null : index[1];
      newBlob = ZERO_ID.test(index[2]) ? null : index[2];
      if (index[3] === SUBMODULE_MODE) noLines = true;
    }
    if (MODE_LINE.exec(text)?.[1] === SUBMODULE_MODE) noLines = true;
  }
  if (noLines) return { oldBlob: null, newBlob: null, oldPath, lines: 'none' };
  // A section merged from several (an Edits file) or with no index line
  // proves only its own bytes: its first section's ids describe none of
  // the rest.
  if ((file.mergedSections ?? 1) > 1 || sections > 1 || !indexed || (oldBlob === null && newBlob === null)) {
    return { oldBlob: null, newBlob: null, oldPath, lines: 'section' };
  }
  return { oldBlob, newBlob, oldPath, lines: 'blob' };
}

async function sectionKey(file: ReviewFile, budget: RelocationBudget): Promise<string | null> {
  budget.spend(file.body.lineCount);
  return await file.body.whenResident(() => file.body.contentKey());
}

/** Relocation's shared allowance: rows scanned and wall time. Spent past
 * either, the editors not yet proven are detached and the diff shows. */
export class RelocationBudget {
  private rows: number;
  private readonly deadline: number;

  constructor(rows: number, ms: number, private readonly now: () => number = () => performance.now()) {
    this.rows = rows;
    this.deadline = now() + ms;
  }

  spend(rows: number): void {
    this.rows -= rows;
    if (this.rows < 0 || this.now() > this.deadline) throw new RelocationBudgetSpent();
  }

  /** Spends `rows` when they fit; false, spending nothing, when not. */
  trySpend(rows: number): boolean {
    if (rows > this.rows || this.now() > this.deadline) return false;
    this.rows -= rows;
    return true;
  }
}

/** The default allowance: a few files of tens of thousands of rows. */
export const RELOCATION_BUDGET_ROWS = 400_000;
export const RELOCATION_BUDGET_MS = 750;

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

/** A row asked for by one coordinate, and the row types that may hold it. */
interface RowRequest {
  by: 'old' | 'new' | 'index';
  value: number;
  accept: readonly RowType[];
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
 * `record`, recording its rows. Reads no text. */
function findRows(file: ReviewFile, requests: readonly RowRequest[], budget: RelocationBudget, record = false): RowTable | null {
  const byOld = new Map<number, RowRequest[]>();
  const byNew = new Map<number, RowRequest[]>();
  const byIndex = new Map<number, RowRequest[]>();
  for (const request of requests) {
    const map = request.by === 'old' ? byOld : request.by === 'new' ? byNew : byIndex;
    const list = map.get(request.value) ?? [];
    list.push(request);
    map.set(request.value, list);
  }
  let table: RowTable | null = null;
  if (record) {
    const capacity = Math.max(16, file.body.lineCount);
    table = { length: 0, type: new Uint8Array(capacity), oldLine: new Int32Array(capacity), newLine: new Int32Array(capacity), lineIndex: new Int32Array(capacity) };
  }
  const walker = new RowWalker(file);
  let walked = 0;
  while (walker.next()) {
    walked += 1;
    if ((walked & 1023) === 0) budget.spend(1024);
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
        if (!request.accept.includes(type)) request.mismatched = true;
        else if ((request.count += 1) === 1) request.row = found;
      }
    };
    if (found.oldLine > 0) consider(byOld.get(found.oldLine));
    if (found.newLine > 0) consider(byNew.get(found.newLine));
    consider(byIndex.get(found.lineIndex));
  }
  budget.spend(walked & 1023);
  return table;
}

// The rows an anchor of each side may sit on (patchRows walkerMatches).
const OLD_ROWS: readonly RowType[] = ['del', 'context'];
const NEW_ROWS: readonly RowType[] = ['add', 'context'];
const CONTEXT_ROWS: readonly RowType[] = ['context'];

function sideRows(side: CommentAnchor['side']): readonly RowType[] {
  return side === 'old' ? OLD_ROWS : side === 'new' ? NEW_ROWS : CONTEXT_ROWS;
}

/** The request locating the row a placed anchor names in its own file. */
function sourceRequest(anchor: CommentAnchor): RowRequest {
  if (anchor.side === 'old') return { by: 'old', value: anchor.oldLine ?? 0, accept: OLD_ROWS, row: null, mismatched: false, count: 0 };
  if (anchor.side === 'new') return { by: 'new', value: anchor.newLine ?? 0, accept: NEW_ROWS, row: null, mismatched: false, count: 0 };
  return { by: 'new', value: anchor.newLine ?? 0, accept: CONTEXT_ROWS, row: null, mismatched: false, count: 0 };
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

/** The anchor a proven row gives an editor of `side`; LEFT stays LEFT. */
function anchorAt(filePath: string, side: CommentAnchor['side'], selectedText: string, row: FoundRow): CommentAnchor {
  if (side === 'old') return { filePath, side, oldLine: row.oldLine, selectedText };
  if (side === 'new') return { filePath, side, newLine: row.newLine, selectedText };
  return { filePath, side, oldLine: row.oldLine, newLine: row.newLine, selectedText };
}

function rowText(row: Pick<FoundRow, 'type'>, raw: string): string {
  return row.type === 'add' || row.type === 'del' ? raw.slice(1) : raw;
}

/** A row's line number on `side` (the new side for `context`). */
function sideLine(side: CommentAnchor['side'], oldLine: number, newLine: number): number {
  return side === 'old' ? oldLine : newLine;
}

/**
 * The row a detached editor renders after, by presentation only: the one
 * row on its side whose text equals its text (when `texts` can be read),
 * else the row with its line number on that side, else the nearest one by
 * that number. -1 when the file has no row on its side.
 */
function suggestRow(table: RowTable, side: CommentAnchor['side'], line: number, selectedText: string, text: ((lineIndex: number) => string) | null): number {
  const accept = sideRows(side).map((type) => ROW_TYPES.indexOf(type));
  let same = -1;
  let sameCount = 0;
  let exact = -1;
  let nearest = -1;
  let distance = Infinity;
  for (let at = 0; at < table.length; at += 1) {
    if (!accept.includes(table.type[at])) continue;
    const number = sideLine(side, table.oldLine[at], table.newLine[at]);
    if (text && rowText({ type: ROW_TYPES[table.type[at]] }, text(table.lineIndex[at])) === selectedText) {
      sameCount += 1;
      same = at;
    }
    if (exact < 0 && number === line) exact = at;
    const off = Math.abs(number - line);
    if (off < distance) {
      distance = off;
      nearest = at;
    }
  }
  if (sameCount === 1) return same;
  return exact >= 0 ? exact : nearest;
}

/**
 * Places each target in `files`, a new diff of the same subject. Placed
 * targets are first proven in the file they are on, which must still be
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
  const identities = new Map<ReviewFile, Promise<FileIdentity>>();
  const identityOf = (file: ReviewFile): Promise<FileIdentity> => {
    let identity = identities.get(file);
    if (!identity) {
      identity = fileIdentity(file, budget);
      identities.set(file, identity);
    }
    return identity;
  };
  const check = (): void => {
    if (cancelled()) throw new RelocationCancelled();
  };
  const detachRest = (): void => {
    for (const [key, target] of targets) {
      if (!out.has(key)) out.set(key, { filePath: target.filePath, anchor: null, proof: proofs.get(key) ?? target.proof, reason: 'line-changed', suggested: null });
    }
  };

  try {
    // Prove each placed target in its own file: one walk per file.
    const bySource = new Map<ReviewFile, K[]>();
    for (const [key, target] of targets) {
      if (target.side === 'file') proofs.set(key, { kind: 'file' });
      else if (target.source) bySource.set(target.source.file, [...(bySource.get(target.source.file) ?? []), key]);
      else proofs.set(key, target.proof);
    }
    for (const [file, keys] of bySource) {
      const requests = keys.map((key) => sourceRequest(targets.get(key)!.source!.anchor));
      findRows(file, requests, budget);
      const identity = await identityOf(file);
      check();
      const key = identity.lines === 'section' ? await sectionKey(file, budget) : null;
      check();
      keys.forEach((targetKey, index) => {
        const row = requests[index].row;
        // No row in the loaded file: an expansion supplied it. A merged
        // file can repeat a coordinate across its sections: which row the
        // anchor named is unknown.
        if (!row || identity.lines === 'none' || requests[index].count > 1) proofs.set(targetKey, NO_PROOF);
        else if (identity.lines === 'section') proofs.set(targetKey, key === null ? NO_PROOF : { kind: 'section', key, lineIndex: row.lineIndex });
        else proofs.set(targetKey, { kind: 'blob', oldBlob: identity.oldBlob, newBlob: identity.newBlob, oldLine: row.oldLine, newLine: row.newLine, context: row.type === 'context' });
      });
    }

    // Find each target's file in the new diff, by path or by the path a
    // rename came from.
    const byPath = new Map(files.map((file) => [file.path, file]));
    let renamed: Map<string, ReviewFile> | null = null;
    const byTarget = new Map<ReviewFile, K[]>();
    for (const [key, target] of targets) {
      let file = byPath.get(target.filePath);
      if (!file) {
        if (renamed === null) {
          renamed = new Map();
          for (const candidate of files) {
            if (candidate.kind !== 'renamed') continue;
            const from = (await identityOf(candidate)).oldPath;
            check();
            if (from !== null) renamed.set(from, candidate);
          }
        }
        file = renamed.get(target.filePath);
      }
      const proof = proofs.get(key)!;
      if (!file) out.set(key, { filePath: target.filePath, anchor: null, proof, reason: 'file-gone', suggested: null });
      else if (proof.kind === 'file') out.set(key, { filePath: file.path, anchor: { filePath: file.path, side: 'file', selectedText: '' }, proof, suggested: null });
      else byTarget.set(file, [...(byTarget.get(file) ?? []), key]);
    }

    // Locate the proven lines: one walk per file, then the text of the
    // rows found, for the guard.
    for (const [file, keys] of byTarget) {
      const identity = await identityOf(file);
      check();
      const needsKey = identity.lines === 'section' && keys.some((key) => proofs.get(key)!.kind === 'section');
      const key = needsKey ? await sectionKey(file, budget) : null;
      check();
      const requests = keys.map((targetKey) => targetRequest(proofs.get(targetKey)!, targets.get(targetKey)!.side, identity, key));
      const table = findRows(file, requests.filter((request) => request !== null), budget, true)!;
      const located = requests.flatMap((request) => (request?.row ? [request.row.lineIndex] : []));
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
        if (!target.guard || !sameGuard(target.guard, lineGuard(rowText(row, texts.get(row.lineIndex) ?? '')))) return detach('line-changed');
        const next: EditorProof = proof.kind === 'section'
          ? proof
          : { kind: 'blob', oldBlob: identity.oldBlob, newBlob: identity.newBlob, oldLine: row.oldLine, newLine: row.newLine, context: row.type === 'context' };
        out.set(targetKey, { filePath: file.path, anchor: anchorAt(file.path, target.side, target.selectedText, row), proof: next, suggested: null });
      });
      if (detached.length > 0) await suggestRows(file, table, detached, targets, out, budget);
      check();
    }
  } catch (err) {
    if (err instanceof RelocationCancelled) throw err;
    if (!(err instanceof RelocationBudgetSpent)) throw err;
  }
  detachRest();
  return out;
}

// Gives the detached targets of `file` their suggested rows. Text is read
// for the text rule only when the whole file fits the budget; the rows
// suggested are read for their quote.
async function suggestRows<K>(
  file: ReviewFile,
  table: RowTable,
  keys: readonly K[],
  targets: ReadonlyMap<K, RelocationTarget>,
  out: Map<K, RelocationOutcome>,
  budget: RelocationBudget,
): Promise<void> {
  const byText = budget.trySpend(table.length);
  const pick = (text: ((lineIndex: number) => string) | null): number[] =>
    keys.map((key) => {
      const target = targets.get(key)!;
      return suggestRow(table, target.side, target.line, target.selectedText, text);
    });
  let rows: number[];
  let quotes: Map<number, string>;
  if (byText) {
    [rows, quotes] = await file.body.whenResident(() => {
      const read = (lineIndex: number): string => file.body.text(lineIndex);
      const picked = pick(read);
      return [picked, new Map(picked.filter((at) => at >= 0).map((at) => [at, read(table.lineIndex[at])]))] as const;
    });
  } else {
    rows = pick(null);
    const found = rows.filter((at) => at >= 0);
    const ranges = found.flatMap((at) => file.body.slice(table.lineIndex[at], table.lineIndex[at] + 1));
    quotes = await whenResident(ranges, () => new Map(found.map((at) => [at, file.body.text(table.lineIndex[at])])));
  }
  keys.forEach((key, index) => {
    const at = rows[index];
    const outcome = out.get(key)!;
    if (at < 0) return;
    const row = tableRow(table, at);
    out.set(key, { ...outcome, suggested: anchorAt(file.path, targets.get(key)!.side, rowText(row, quotes.get(at) ?? ''), row) });
  });
}
