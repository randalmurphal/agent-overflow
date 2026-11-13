import { visibleBody } from './reviewComments';
import type { CommentSuggestion, ReviewComment } from '../types/models';

// A comment body cut at its suggestion fences, by the line ranges the
// backend reports (`CommentSuggestion.bodyStartLine`..`bodyEndLine`, or a
// preview's `ParseCommentSuggestions` blocks): 0-based lines of the body
// split on "\n", each range the fence from its opening line through its
// closing one. The pieces between render as markdown (HTML comments
// removed, as `visibleBody` does), each block as its diff in place.
//
// A body is cut only when every block is placed and the ranges are
// whole lines of the body in order without overlap. Otherwise (GitLab
// reports suggestions it could not match to the body's fences, or a
// range no longer fits the text) the body renders whole, its fences as
// code, and every block's diff follows it.

/** One block's place in its body, or null when it has none. */
export interface BodyRange {
  readonly startLine: number;
  readonly endLine: number;
}

export type BodyPiece =
  | { readonly kind: 'markdown'; readonly text: string; readonly key: string }
  | { readonly kind: 'block'; readonly at: number; readonly key: string };

export interface BodyLayout {
  /** The pieces in body order: markdown and blocks (`at`, an index into
   * the ranges) when the body is cut, else the whole visible body. */
  readonly pieces: readonly BodyPiece[];
  /** Blocks rendered after the body (an uncut body: every one). */
  readonly after: readonly number[];
}

function cuttable(ranges: readonly (BodyRange | null)[], lineCount: number): ranges is readonly BodyRange[] {
  let next = 0;
  for (const range of ranges) {
    if (range === null || !Number.isInteger(range.startLine) || !Number.isInteger(range.endLine)) return false;
    if (range.startLine < next || range.endLine < range.startLine || range.endLine >= lineCount) return false;
    next = range.endLine + 1;
  }
  return true;
}

export function layoutBody(body: string, ranges: readonly (BodyRange | null)[]): BodyLayout {
  const lines = body.split('\n');
  if (ranges.length === 0 || !cuttable(ranges, lines.length)) {
    const whole = visibleBody(body);
    return { pieces: whole === '' ? [] : [{ kind: 'markdown', text: whole, key: 'body' }], after: ranges.map((_, at) => at) };
  }
  const pieces: BodyPiece[] = [];
  let from = 0;
  ranges.forEach((range, at) => {
    const text = visibleBody(lines.slice(from, range.startLine).join('\n'));
    if (text !== '') pieces.push({ kind: 'markdown', text, key: `before:${at}` });
    pieces.push({ kind: 'block', at, key: `block:${at}` });
    from = range.endLine + 1;
  });
  const tail = visibleBody(lines.slice(from).join('\n'));
  if (tail !== '') pieces.push({ kind: 'markdown', text: tail, key: 'tail' });
  return { pieces, after: [] };
}

/** The text find reads in a body laid out so: its markdown, without the
 * blocks, which render as diffs find skips. */
export function layoutText(layout: BodyLayout): string {
  return layout.pieces.flatMap((piece) => (piece.kind === 'markdown' ? [piece.text] : [])).join('\n\n');
}

/** The text find reads in a sent comment: its visible body without the
 * suggestion blocks placed in it, which render as diffs find skips. */
export function findableBody(comment: Pick<ReviewComment, 'body' | 'suggestions'>): string {
  return layoutText(layoutBody(comment.body, suggestionRanges(comment.suggestions)));
}

/** Each suggestion's place in its comment's body, or null for one the
 * forge could not place. */
export function suggestionRanges(suggestions: readonly CommentSuggestion[] | null | undefined): (BodyRange | null)[] {
  return (suggestions ?? []).map((each) => (each.bodyPlaced ? { startLine: each.bodyStartLine, endLine: each.bodyEndLine } : null));
}
