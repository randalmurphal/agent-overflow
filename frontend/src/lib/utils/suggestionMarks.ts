import { randomId } from './randomId';
import { visibleBody } from './reviewComments';
import type { CommentSuggestion, ReviewComment } from '../types/models';

// A comment body rendered as one Markdown document with its suggestion
// fences claimed in place (ChatMarkdown's `codeClaim`). The backend says
// where each fence is (0-based lines of the body split on "\n", opening
// through closing fence; `CommentSuggestion.bodyStartLine`..`bodyEndLine`,
// or a preview's `ParseCommentSuggestions` blocks). Each placed fence
// whose opening line is a suggestion fence and whose content is exactly
// the suggestion's lines gets a mark appended to its info string; the
// renderer's code island for a marked fence renders the diff, and find's
// projection skips it. The mark carries a random token of this page, so
// a body cannot write one. A block that is unplaced, or whose fence no
// longer matches, leaves its fence as code and renders after the body.

/** A block's place in its body and the lines it proposes. */
export interface PlacedBlock {
  readonly startLine: number;
  readonly endLine: number;
  readonly lines: readonly string[];
}

const MARK = `ao-suggestion-${randomId()}`;
const MARK_AT_END = new RegExp(`(?:^|\\s)${MARK}-(\\d+)$`);
const OPENER = /^( {0,3})(`{3,}|~{3,})(.*)$/;
const SUGGESTION_INFO = /^suggestion(?::-\d+\+\d+)?$/;

/** The block a fence's info string marks, or null. */
export function suggestionMarkIndex(lang: string): number | null {
  const found = MARK_AT_END.exec(lang);
  return found ? Number(found[1]) : null;
}

export function isSuggestionMark(lang: string): boolean {
  return suggestionMarkIndex(lang) !== null;
}

function withoutCR(line: string): string {
  return line.endsWith('\r') ? line.slice(0, -1) : line;
}

function fenceMatches(lines: readonly string[], block: PlacedBlock): boolean {
  const { startLine: start, endLine: end } = block;
  if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end <= start || end >= lines.length) return false;
  const open = OPENER.exec(withoutCR(lines[start]));
  if (!open || !SUGGESTION_INFO.test(open[3].trim())) return false;
  const [, indent, fence] = open;
  const close = withoutCR(lines[end]);
  const closer = new RegExp(`^ {0,3}${fence[0] === '`' ? '`' : '~'}{${fence.length},}[ \\t]*$`);
  if (!closer.test(close)) return false;
  const content = lines.slice(start + 1, end).map((line) => withoutCR(line).replace(new RegExp(`^ {0,${indent.length}}`), ''));
  return content.length === block.lines.length && content.every((line, i) => line === block.lines[i]);
}

export interface MarkedBody {
  /** The body to render: visible (HTML comments removed), its claimed
   * fences marked. */
  readonly source: string;
  /** The blocks rendered after the body, in order. */
  readonly after: readonly number[];
}

export function markSuggestionFences(body: string, blocks: readonly (PlacedBlock | null)[]): MarkedBody {
  const lines = body.split('\n');
  const out = [...lines];
  const marked: number[] = [];
  const used = new Set<number>();
  blocks.forEach((block, at) => {
    if (block === null || used.has(block.startLine) || !fenceMatches(lines, block)) return;
    used.add(block.startLine);
    marked.push(at);
    out[block.startLine] = out[block.startLine].replace(/[ \t]*(\r?)$/, ` ${MARK}-${at}$1`);
  });
  const source = visibleBody(out.join('\n'));
  // A marked fence the visible body dropped (it sat inside an HTML
  // comment) renders after the body like any unplaced block, so no
  // suggestion is ever lost.
  const shown = new Set(marked.filter((at) => new RegExp(`${MARK}-${at}\\r?$`, 'm').test(source)));
  return { source, after: blocks.map((_, at) => at).filter((at) => !shown.has(at)) };
}

/** Each sent suggestion's place and lines, or null for one the forge
 * could not place. */
export function sentBlocks(suggestions: readonly CommentSuggestion[] | null | undefined): (PlacedBlock | null)[] {
  return (suggestions ?? []).map((each) => (each.bodyPlaced ? { startLine: each.bodyStartLine, endLine: each.bodyEndLine, lines: each.to } : null));
}

/** The source find projects for a sent comment: what the pane renders,
 * its claimed fences skipped by `isSuggestionMark`. */
export function findableBody(comment: Pick<ReviewComment, 'body' | 'suggestions'>): string {
  return markSuggestionFences(comment.body, sentBlocks(comment.suggestions)).source;
}
