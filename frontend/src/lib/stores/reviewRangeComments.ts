import type { DiffReviewComment } from '../types/models';
import type { CommentAnchor } from '../utils/reviewRows';
import type { ReviewPRLineIndex } from './reviewPRLineIndex.svelte';

// Comments in a commit run that ends at the PR's head
// (docs/specs/pull-requests.md#commit-ranges). The run shows its lines as
// they are at the head, so its comment home is the PR's (the same drafts,
// batch and "Comment now" as All commits): a new-side line inside a hunk
// of the whole PR's diff takes a PR comment, saved in the whole diff's
// numbers; any other new-side line takes a draft for the agent only,
// saved by its head line alone, which a PR review leaves out. An old-side
// line names the run's base, which the PR's diff does not show, so it
// takes no comment. Until the whole diff's index is ready no line takes a
// PR comment.

/** What a line of the view takes: a PR comment, an agent-only draft, or
 * nothing. */
export type LineCommentTarget = 'pr' | 'agent' | 'none';

export const RANGE_LINE_NOT_IN_PR = 'This line is not part of the pull request\'s diff, so a comment here goes to the agent only.';
export const RANGE_LINE_PR_LOADING = 'The pull request\'s diff is still loading, so a comment here goes to the agent only for now.';
export const RANGE_OLD_LINE = 'A removed line of this range is not in the pull request\'s diff, so it takes no comment here.';

/** The head line a run's anchor names: an unchanged or added row's new
 * number, from either half of a split row; 0 for a removed row. */
function headLine(anchor: Pick<CommentAnchor, 'newLine'>): number {
  return anchor.newLine && anchor.newLine > 0 ? anchor.newLine : 0;
}

export function lineCommentTarget(
  index: ReviewPRLineIndex,
  anchor: Pick<CommentAnchor, 'filePath' | 'newLine' | 'side'>,
  agentDrafts: boolean,
): LineCommentTarget {
  if (anchor.side === 'file') return 'none';
  const line = headLine(anchor);
  if (line === 0) return 'none';
  if (index.state === 'ready' && index.resolveNewLine(anchor.filePath, line)) return 'pr';
  return agentDrafts ? 'agent' : 'none';
}

/** Why a line of the view takes what it does, as its control's hover
 * text; null for a PR line. */
export function lineCommentReason(index: ReviewPRLineIndex, target: LineCommentTarget, anchor: Pick<CommentAnchor, 'newLine'>): string | null {
  if (target === 'pr') return null;
  if (headLine(anchor) === 0) return RANGE_OLD_LINE;
  return index.state === 'ready' ? RANGE_LINE_NOT_IN_PR : RANGE_LINE_PR_LOADING;
}

/**
 * The anchor a comment from the run is saved with: a PR line in the whole
 * diff's numbers (an unchanged line with both, as a comment there is), an
 * agent-only line by its head number alone. Null when the line takes no
 * comment.
 */
export function rangeSaveAnchor(index: ReviewPRLineIndex, anchor: CommentAnchor, agentDrafts: boolean): { anchor: CommentAnchor; target: 'pr' | 'agent' } | null {
  const target = lineCommentTarget(index, anchor, agentDrafts);
  if (target === 'none') return null;
  const line = headLine(anchor);
  const resolved = target === 'pr' ? index.resolveNewLine(anchor.filePath, line) : null;
  return {
    target,
    anchor: {
      filePath: anchor.filePath,
      side: 'new',
      newLine: line,
      oldLine: resolved?.oldLine,
      selectedText: anchor.selectedText,
    },
  };
}

/**
 * The PR's drafts as the run's rows place them: by head line alone (the
 * row may be unchanged in the run where it is added in the PR, or the
 * other way round). A draft on a removed line of the whole diff, or a
 * file comment, has no row in the run and is not shown there; the send
 * strip still counts it, and All commits shows it.
 */
export function rangeShownDrafts(drafts: readonly DiffReviewComment[]): DiffReviewComment[] {
  const out: DiffReviewComment[] = [];
  for (const draft of drafts) {
    if (draft.side === 'reply') {
      out.push(draft);
      continue;
    }
    if (draft.side === 'file' || !draft.newLine) continue;
    out.push({
      ...draft,
      side: 'new',
      oldLine: undefined,
      startOldLine: undefined,
      startNewLine: draft.startNewLine && draft.startNewLine > 0 ? draft.startNewLine : undefined,
    });
  }
  return out;
}

/** The drafts a PR review from the run leaves out: those whose line is
 * not a row of the whole diff (agent-only drafts). All of them while the
 * index is not ready. */
export function rangePROrphans(index: ReviewPRLineIndex, drafts: readonly DiffReviewComment[]): Set<string> {
  const out = new Set<string>();
  for (const draft of drafts) {
    if (draft.side === 'reply') continue;
    if (!index.hasAnchor(draft.filePath, { side: draft.side, oldLine: draft.oldLine, newLine: draft.newLine })) out.add(draft.id);
  }
  return out;
}

/** What a diff row asks of a line: the pane's `lineComment`. */
export type LineCommentGate = (anchor: Pick<CommentAnchor, 'filePath' | 'newLine' | 'side'>) => { target: LineCommentTarget; reason: string | null } | null;
