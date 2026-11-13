import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import type { EditorRow } from './reviewRows';
import type { DiffReviewComment } from '../types/models';

// The exact current text of new-side lines, read from the diff the pane
// holds (never a re-typed copy, never a forge fetch): what `Suggest
// change` prefills and what a GitHub suggestion replaces (GitHub sends no
// replaced text). Only lines the loaded diff shows, all of them in one
// unbroken run of the file's rows, answer; anything else is unknown.

/** The line indexes of new-side lines `from`..`to` in `file`, in order,
 * or null when the diff does not show every one of them in one run. */
export function newSideLineIndexes(file: ReviewFile, from: number, to: number): number[] | null {
  if (from <= 0 || to < from) return null;
  const out: number[] = [];
  const walker = new RowWalker(file);
  while (walker.next()) {
    if (walker.type !== 'add' && walker.type !== 'context') {
      // A removed row or a marker between them is still the hunk's run; a
      // gap ends it.
      if (out.length > 0 && walker.type === 'gap') break;
      continue;
    }
    const line = walker.newLine;
    if (line < from) continue;
    if (line > to) break;
    if (line !== from + out.length) return null;
    out.push(walker.lineIndex);
  }
  return out.length === to - from + 1 ? out : null;
}

/** A diff line's text without its `+` or ` ` column and without a CRLF
 * file's line ending (a CR inside the line is content). */
function newSideText(raw: string): string {
  const text = raw.startsWith('+') || raw.startsWith(' ') ? raw.slice(1) : raw;
  return text.endsWith('\r') ? text.slice(0, -1) : text;
}

/** Reads new-side lines `from`..`to` of `file`, bringing evicted text back
 * first; null when the diff does not show them. Rejects with
 * PatchTextLost when their text can no longer be read. */
export async function readNewSideLines(file: ReviewFile, from: number, to: number): Promise<string[] | null> {
  const indexes = newSideLineIndexes(file, from, to);
  if (!indexes || indexes.length === 0) return null;
  const first = indexes[0];
  const last = indexes[indexes.length - 1];
  return file.body.whenResident(() => indexes.map((index) => newSideText(file.body.text(index))), first, last + 1);
}

/** New-side lines a comment is written on: its path and span. */
export interface NewSideSpan {
  readonly path: string;
  readonly from: number;
  readonly to: number;
}

const NEW_HALF = new Set(['new', 'context']);

/** The new-side lines an open new-comment editor is on: its line, or the
 * range it spans; null on a removed line, a file comment, a range on the
 * old half, or an editor that is not placed on its own line. */
export function editorNewSideSpan(editor: Pick<EditorRow, 'state' | 'repeated' | 'anchor' | 'range' | 'filePath'>): NewSideSpan | null {
  const anchor = editor.anchor;
  if (editor.state !== 'placed' || editor.repeated || !anchor || !NEW_HALF.has(anchor.side)) return null;
  const to = anchor.newLine ?? 0;
  if (to <= 0) return null;
  if (editor.range && (editor.range.half !== 'new' || editor.range.crossesHalves)) return null;
  const from = editor.range ? editor.range.from : to;
  return from > 0 && from <= to ? { path: editor.filePath, from, to } : null;
}

/** The new-side lines a draft is on, as `editorNewSideSpan` reads an editor. */
export function draftNewSideSpan(draft: Pick<DiffReviewComment, 'filePath' | 'side' | 'newLine' | 'startNewLine' | 'startOldLine'>): NewSideSpan | null {
  const to = draft.newLine ?? 0;
  if (!NEW_HALF.has(draft.side) || to <= 0 || ((draft.startOldLine ?? 0) > 0 && !(draft.startNewLine ?? 0))) return null;
  const from = draft.startNewLine && draft.startNewLine > 0 ? draft.startNewLine : to;
  return from <= to ? { path: draft.filePath, from, to } : null;
}
