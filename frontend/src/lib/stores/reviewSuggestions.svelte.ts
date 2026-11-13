import { ApplyPRSuggestion } from './bindings';
import type { SuggestionApply } from '../../../bindings/agent-overflow/internal/git/models';
import { threadWhere } from './reviewCommentWrites.svelte';
import type { ForgeOps } from './reviewForgeOps.svelte';
import { prReferenceWire } from '../utils/prReference';
import type { ReviewFile } from '../utils/patchStore';
import { newSideLineIndexes } from '../utils/suggestionLines';
import type { CommentSuggestion, ReviewComment, ReviewThread } from '../types/models';

// Suggested changes in a review pane (docs/specs/pull-requests.md,
// "Suggestions, reactions and preview"): Apply on one suggestion, and the diff text the
// read and the editors take a suggestion's replaced lines from. Apply is
// one operation per suggestion (reviewForgeOps.svelte.ts), named with
// the head the pane's diff was loaded at; a moved branch is the forge's
// refusal. After an apply the head moves and the pane's stale-head flow
// takes over; nothing here shows the change before the forge does.

export interface ReviewSuggestions {
  /** The backend reads and applies suggestions (pr-suggestions-reactions.v1). */
  readonly available: boolean;
  /** Why Apply is withheld now (the now writes' reason), or null. */
  readonly withheld: string | null;
  pending(thread: ReviewThread, comment: ReviewComment, suggestion: Pick<CommentSuggestion, 'index'>): boolean;
  error(thread: ReviewThread, comment: ReviewComment, suggestion: Pick<CommentSuggestion, 'index'>): string | null;
  apply(thread: ReviewThread, comment: ReviewComment, suggestion: CommentSuggestion): Promise<void>;
  /** The file of the PR's whole diff at its live head, whose new-side
   * text is the text a suggestion on `path` replaces; null when the diff
   * on screen is another (a picked commit, a stale head, still loading)
   * or does not hold the file. */
  currentFile(path: string): ReviewFile | null;
  /** Where a suggestion written on the thread reads the lines it would
   * replace (an editor's preview): its new-side line or range in the
   * current diff, or null (an old-side, outdated or file thread). */
  threadSource(thread: ReviewThread): { file: ReviewFile; from: number; to: number } | null;
  /** New-side lines `from`..`to` of `path` in the current diff, when the
   * diff shows every one of them: what `Suggest change` prefills and an
   * editor's preview replaces. */
  lineSource(path: string, from: number, to: number): { file: ReviewFile; from: number; to: number } | null;
}

/** A thread's new-side line or range, or null when it is not on the new
 * side (a removed line, a range from one, a file or PR comment). */
export function threadNewSideLines(thread: Pick<ReviewThread, 'path' | 'line' | 'startLine' | 'side' | 'startSide' | 'isOutdated'>): { from: number; to: number } | null {
  const oldSide = (side: string | undefined) => side === 'left' || side === 'LEFT' || side === 'old';
  if (thread.isOutdated || thread.path === '' || !thread.line || oldSide(thread.side) || oldSide(thread.startSide)) return null;
  const from = thread.startLine && thread.startLine < thread.line ? thread.startLine : thread.line;
  return { from, to: thread.line };
}

export interface ReviewSuggestionsHost {
  readonly ops: ForgeOps;
  capable(): boolean;
  onPR(): boolean;
  withheld(): string | null;
  /** The head the pane's diff was loaded at. */
  loadedHeadSHA(): string;
  /** The diff on screen is the PR's whole diff at its live head. */
  diffIsCurrentHead(): boolean;
  files(): readonly ReviewFile[];
}

function suggestionKey(thread: ReviewThread, comment: ReviewComment, index: number): string {
  return `suggestion:${thread.id}\u0000${comment.id}\u0000${index}`;
}

/** The apply request, made from the suggestion the reader pressed Apply
 * on: what names it is the forge's read of it, never a copy kept apart. */
export function applyRequest(thread: ReviewThread, comment: ReviewComment, suggestion: CommentSuggestion, headSha: string): SuggestionApply {
  return {
    target: { threadId: thread.id, commentId: comment.id },
    index: suggestion.index,
    suggestionId: suggestion.id || undefined,
    headSha,
  };
}

export function createReviewSuggestions(host: ReviewSuggestionsHost): ReviewSuggestions {
  const available = $derived(host.capable() && host.onPR());
  const filesByPath = $derived.by(() => {
    if (!host.diffIsCurrentHead()) return null;
    const out = new Map<string, ReviewFile>();
    for (const file of host.files()) out.set(file.path, file);
    return out;
  });

  function lineSource(path: string, from: number, to: number): { file: ReviewFile; from: number; to: number } | null {
    const file = filesByPath?.get(path) ?? null;
    return file && newSideLineIndexes(file, from, to) !== null ? { file, from, to } : null;
  }

  return {
    get available() { return available; },
    get withheld() { return host.withheld(); },
    pending: (thread, comment, suggestion) => host.ops.pending(suggestionKey(thread, comment, suggestion.index)),
    error: (thread, comment, suggestion) => host.ops.error(suggestionKey(thread, comment, suggestion.index)),
    async apply(thread, comment, suggestion) {
      if (!available || !suggestion.appliable || suggestion.applied || host.withheld() !== null) return;
      const key = suggestionKey(thread, comment, suggestion.index);
      const request = applyRequest(thread, comment, suggestion, host.loadedHeadSHA());
      // A refusal because the branch or the suggestion moved is fixed by
      // reading the PR again; every refusal reads it, nothing was written.
      await host.ops.run(key, key, (ref) => ApplyPRSuggestion(prReferenceWire(ref), request), {
        subject: `The suggestion on ${threadWhere(thread)}`,
        notDone: 'was not applied',
      }, { refreshOnFailure: true });
    },
    currentFile: (path) => filesByPath?.get(path) ?? null,
    threadSource(thread) {
      const lines = threadNewSideLines(thread);
      return lines ? lineSource(thread.path, lines.from, lines.to) : null;
    },
    lineSource,
  };
}
