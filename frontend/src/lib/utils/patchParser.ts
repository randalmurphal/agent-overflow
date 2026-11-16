import { formatSourceKey } from './diffSourceKey';
import { appendFNV1a32, FNV_OFFSET_BASIS_32 } from './fnv1a';
import { PatchBody, type BodySegment } from './patchBody';
import {
  cleanPath,
  HUNK_ADDED,
  HUNK_CONTEXT,
  HUNK_HEADER,
  HUNK_OUTSIDE,
  HUNK_REMOVED,
  HunkBody,
  PATCH_META_PREFIXES,
  type PatchFile,
} from './patchFiles';
import {
  keptText,
  LINE_ADD,
  LINE_CONTEXT,
  LINE_DEL,
  LINE_META,
  PatchStore,
  type LineClass,
  type PatchChunkSpan,
  type PatchLineCallback,
  type PatchTextKey,
} from './patchStore';

// `PatchParser` classifies lines as they complete and splits the patch
// into files with the same rules as `parsePatchFiles`; each file is a
// `PatchBody` (`patchBody.ts`).

/** A chunk's byte range and key as the backend answers them
 * (ReviewDiffChunk); a backend older than the key answers none.
 * PatchParser.append checks them and stores the chunk under the
 * PatchChunkSpan they make. */
export interface PatchChunkMarks {
  offset: number;
  nextOffset: number;
  key?: PatchTextKey;
}

/** The hash a chunk without a key runs through for the source key. An
 * object so a test can see that a keyed chunk is not hashed. */
export const sourceHash = { append: appendFNV1a32 };

/** The file-level facts every file list, header and tree reads. */
export interface DiffFileSummary {
  path: string;
  kind: string;
  additions: number;
  deletions: number;
  /** Conflict-region count for `kind === 'conflict'` pseudo-files. */
  conflicts?: number;
  /** Structural-conflict badge for conflict pseudo-files with no
   * textual regions, e.g. "modify/delete". */
  conflictLabel?: string;
}

/** One file of the review surface: its summary and its compact lines. */
export interface ReviewFile extends DiffFileSummary {
  readonly body: PatchBody;
  /** New-side file length, learned from a context expansion. Sizes (or
   * retires) the trailing gap. */
  newSideTotal?: number;
  /** Skip hunk-gap rows (see PatchFile.suppressGaps). */
  suppressGaps?: boolean;
  /** How many edit sections of one path the Edits scope joined into this
   * file. Over 1, its first section's index line does not describe it
   * (utils/editorPlacement.ts). */
  mergedSections?: number;
  /** What the metadata lines leading its body say, read as they were
   * parsed, so a reader of them needs none of its text. Absent: no
   * leading metadata. */
  header?: PatchHeader;
}

/** What a file's leading metadata lines say (utils/editorPlacement.ts). */
export interface PatchHeader {
  /** `diff --git` lines: over 1 only for sections joined into one. */
  sections: number;
  /** `rename from`: the path the file had before. */
  oldPath: string | null;
  /** Whether an `index` line names the sides' blob ids. */
  indexed: boolean;
  /** The `index` line's ids; a zero id is none. */
  oldBlob: string | null;
  newBlob: string | null;
  /** A mode line or the `index` line gives a gitlink's mode. */
  submodule: boolean;
  /** git said the file is binary (`Binary files … differ`): its body has
   * that line and no hunk. */
  binary: boolean;
}

const ZERO_ID = /^0+$/;
const INDEX_LINE = /^index ([0-9a-f]+)\.\.([0-9a-f]+)(?: ([0-7]+))?$/;
const MODE_LINE = /^(?:new file|deleted file|old|new) mode ([0-7]+)$/;
const SUBMODULE_MODE = '160000';

function emptyHeader(): PatchHeader {
  return { sections: 0, oldPath: null, indexed: false, oldBlob: null, newBlob: null, submodule: false, binary: false };
}

/** Records one leading metadata line in `header`. */
function readHeaderLine(header: PatchHeader, text: string): void {
  if (text.startsWith('diff --git ')) header.sections += 1;
  else if (text.startsWith('rename from ')) header.oldPath = keptText.own(text.slice('rename from '.length));
  const index = INDEX_LINE.exec(text);
  if (index) {
    header.indexed = true;
    header.oldBlob = ZERO_ID.test(index[1]) ? null : keptText.own(index[1]);
    header.newBlob = ZERO_ID.test(index[2]) ? null : keptText.own(index[2]);
    if (index[3] === SUBMODULE_MODE) header.submodule = true;
  }
  if (MODE_LINE.exec(text)?.[1] === SUBMODULE_MODE) header.submodule = true;
}

/** A review file over a parsed PatchFile (conflict pseudo-files). */
export function reviewFileFromPatchFile(file: PatchFile): ReviewFile {
  const out: ReviewFile = {
    path: file.path,
    kind: file.kind,
    additions: file.additions,
    deletions: file.deletions,
    body: PatchBody.fromPatchLines(file.lines),
  };
  const header = emptyHeader();
  let index = 0;
  for (; index < file.lines.length && file.lines[index].type === 'meta'; index += 1) readHeaderLine(header, file.lines[index].content);
  if (isBinaryLine(file.lines[index]?.content ?? '')) header.binary = true;
  if (file.lines.length > 0 && file.lines[0].type === 'meta') out.header = header;
  if (file.conflicts !== undefined) out.conflicts = file.conflicts;
  if (file.conflictLabel !== undefined) out.conflictLabel = file.conflictLabel;
  if (file.newSideTotal !== undefined) out.newSideTotal = file.newSideTotal;
  if (file.suppressGaps) out.suppressGaps = true;
  return out;
}

/** A PatchFile over a review file's lines, for surfaces that render one
 * whole file with the PatchFile renderers. */
export function patchFileFromReviewFile(file: ReviewFile): PatchFile {
  const out: PatchFile = {
    path: file.path,
    kind: file.kind,
    additions: file.additions,
    deletions: file.deletions,
    lines: file.body.toPatchLines(),
  };
  if (file.newSideTotal !== undefined) out.newSideTotal = file.newSideTotal;
  if (file.suppressGaps) out.suppressGaps = true;
  return out;
}

interface Section {
  /** Copied from the chunk text it was read in (keptText), so a section
   * that spans later chunks keeps none of that text. */
  path: string;
  kind: string;
  additions: number;
  deletions: number;
  start: number;
  end: number;
  /** First hunk header, or -1. */
  firstHunk: number;
  header: PatchHeader;
  /** Whether every line so far is metadata: the header is still open. */
  leading: boolean;
}

/**
 * Splits patch text into review files as it arrives. The rules are
 * `parsePatchFiles`': a file starts at each `diff --git` line, its path
 * and kind come from its header lines, and a type change (the same path
 * deleted then added in adjacent sections) is one file.
 *
 * `files` grows as files complete. A deleted file is held back until the
 * next section shows whether it is half of a type change, so a published
 * file is final.
 */
export class PatchParser {
  readonly store = new PatchStore();
  readonly files: ReviewFile[] = [];
  private current: Section | null = null;
  private held: Section | null = null;
  private readonly body = new HunkBody();
  private readonly finished: ([Section] | [Section, Section])[] = [];
  // The source key of the text so far: the last chunk's key, or the hash
  // continued over each chunk that came without one.
  private hash = FNV_OFFSET_BASIS_32;
  private units = 0;
  private ended = false;
  private readonly onLine: PatchLineCallback = (line, text, offset, length) => this.classifyLine(line, text, offset, length);

  /** Appends patch text, which may end anywhere. `chunk` is what the
   * backend answered with the text (PatchChunkMarks): the byte range for
   * a store that can evict it, `key` for the source key through the
   * chunk's end. A chunk with a key is not hashed. A key the chunk cannot
   * have is refused before any of the chunk is stored. */
  append(data: string, chunk?: PatchChunkMarks): void {
    if (this.ended) throw new Error('patch parser: append after end');
    if (data === '') return;
    const span = chunk === undefined ? undefined : this.checkSpan(data, chunk);
    this.store.append(data, this.onLine, span);
    const key = span?.key;
    if (key !== undefined) {
      this.hash = key.hash;
      this.units = key.units;
    } else {
      this.hash = sourceHash.append(this.hash, data);
      this.units += data.length;
    }
    this.publish();
  }

  // The one check of what a chunk arrives with, and the span it is stored
  // under. The key comes off the wire, so its type is checked too.
  private checkSpan(data: string, chunk: PatchChunkMarks): PatchChunkSpan {
    const { offset, nextOffset, key } = chunk;
    if (key !== undefined) this.checkKey(key, data);
    return { offset, nextOffset, key };
  }

  // A key is cumulative and a chunk's text is its units, so its key
  // counts exactly those more.
  private checkKey(key: PatchTextKey, data: string): void {
    const valid = typeof key === 'object' && key !== null
      && Number.isInteger(key.hash) && key.hash >= 0 && key.hash < 0x1_0000_0000
      && Number.isSafeInteger(key.units)
      && key.units === this.units + data.length;
    if (!valid) throw new Error('patch parser: a chunk came with a key that cannot be its text\'s');
  }

  /** Marks the end of the patch and publishes the last files. */
  end(): void {
    if (this.ended) return;
    this.ended = true;
    this.store.end(this.onLine);
    this.finishSection(this.store.lineCount);
    if (this.held) {
      this.finished.push([this.held]);
      this.held = null;
    }
    this.publish();
  }

  get complete(): boolean {
    return this.ended;
  }

  /** The review comment source key of the whole patch: `diffSourceKey`
   * of its text. Empty for an empty patch and before the end. */
  sourceKey(): string {
    if (!this.ended || this.units === 0) return '';
    return formatSourceKey(this.hash, this.units);
  }

  private classifyLine(line: number, text: string, offset: number, length: number): LineClass {
    const kind = this.body.next(text, offset, length);
    if (startsAt(text, offset, length, 'diff --git ')) {
      this.finishSection(line);
      const header = text.slice(offset, offset + length);
      const parts = header.split(/\s+/);
      this.current = {
        path: keptText.own(cleanPath(parts[3] ?? '')),
        kind: 'modified',
        additions: 0,
        deletions: 0,
        start: line,
        end: line,
        firstHunk: -1,
        header: emptyHeader(),
        leading: true,
      };
      readHeaderLine(this.current.header, header);
      return LINE_META;
    }
    const current = this.current;
    // parsePatchFiles' rule: a hunk body's lines by its header's counts,
    // and a line outside every body by its prefix.
    const outside = kind === HUNK_OUTSIDE;
    const plus = kind === HUNK_ADDED
      || (outside && length > 0 && text.charCodeAt(offset) === 43 && !startsAt(text, offset, length, '+++'));
    const minus = kind === HUNK_REMOVED
      || (outside && length > 0 && text.charCodeAt(offset) === 45 && !startsAt(text, offset, length, '---'));
    if (current) {
      if (startsAt(text, offset, length, 'new file')) current.kind = 'added';
      if (startsAt(text, offset, length, 'deleted file')) current.kind = 'deleted';
      if (startsAt(text, offset, length, 'rename from ')) current.kind = 'renamed';
      if (startsAt(text, offset, length, 'rename to ')) {
        current.path = keptText.own(cleanPath(text.slice(offset + 'rename to '.length, offset + length)));
      }
      if (outside && startsAt(text, offset, length, '+++ ')) {
        const next = cleanPath(text.slice(offset + 4, offset + length));
        if (next && next !== '/dev/null') current.path = keptText.own(next);
      }
      if (plus) current.additions += 1;
      if (minus) current.deletions += 1;
    }
    let lineClass: LineClass;
    if (plus) {
      lineClass = LINE_ADD;
    } else if (minus) {
      lineClass = LINE_DEL;
    } else if (kind === HUNK_CONTEXT) {
      lineClass = LINE_CONTEXT;
    } else if (kind === HUNK_HEADER && this.body.header) {
      lineClass = this.body.header;
      if (current && current.firstHunk < 0) current.firstHunk = line;
    } else if (kind !== HUNK_OUTSIDE || PATCH_META_PREFIXES.some((prefix) => startsAt(text, offset, length, prefix))) {
      // A malformed hunk header, the no-newline marker, or a file header.
      if (current?.leading) readHeaderLine(current.header, text.slice(offset, offset + length));
      return LINE_META;
    } else {
      // git's one line for a binary file stays a line of the body, as it
      // always rendered; the header says what it is.
      if (current?.leading && isBinaryLine(text.slice(offset, offset + Math.min(length, BINARY_PREFIX_MAX)))) current.header.binary = true;
      lineClass = LINE_CONTEXT;
    }
    if (current) current.leading = false;
    return lineClass;
  }

  // Sections finish while a chunk is being indexed, so their files are
  // built by publish() once the store can read the chunk's lines.
  private finishSection(end: number): void {
    const section = this.current;
    if (!section) return;
    this.current = null;
    section.end = end;
    if (!section.path) return;
    const held = this.held;
    if (held) {
      this.held = null;
      if (held.path === section.path && section.kind === 'added') {
        this.finished.push([held, section]);
        return;
      }
      this.finished.push([held]);
    }
    if (section.kind === 'deleted') {
      this.held = section;
      return;
    }
    this.finished.push([section]);
  }

  private publish(): void {
    for (const sections of this.finished) {
      this.files.push(sections.length === 2 ? this.typeChangeFile(sections[0], sections[1]) : this.sectionFile(sections[0]));
    }
    this.finished.length = 0;
  }

  private sectionFile(section: Section): ReviewFile {
    return {
      path: section.path,
      kind: section.kind,
      additions: section.additions,
      deletions: section.deletions,
      body: new PatchBody([{ store: this.store, start: section.start, end: section.end }]),
      header: section.header,
    };
  }

  // The created section's header block is dropped: meta rows never render,
  // and one file carries one preamble. Both sides are present whole, so no
  // gap is shown between the deletion and creation hunks.
  private typeChangeFile(deleted: Section, added: Section): ReviewFile {
    const segments: BodySegment[] = [{ store: this.store, start: deleted.start, end: deleted.end }];
    if (added.firstHunk >= 0) segments.push({ store: this.store, start: added.firstHunk, end: added.end });
    return {
      path: deleted.path,
      kind: 'modified',
      additions: deleted.additions + added.additions,
      deletions: deleted.deletions + added.deletions,
      body: new PatchBody(segments),
      suppressGaps: true,
      header: deleted.header,
    };
  }
}

const BINARY_PREFIX_MAX = 16;
function isBinaryLine(text: string): boolean {
  return text.startsWith('Binary files ') || text.startsWith('GIT binary patch');
}

function startsAt(text: string, offset: number, length: number, prefix: string): boolean {
  return length >= prefix.length && text.startsWith(prefix, offset);
}

/** Parses a whole patch held in memory. */
export function parseReviewFiles(patch: string): ReviewFile[] {
  const parser = new PatchParser();
  parser.append(patch);
  parser.end();
  return parser.files;
}
