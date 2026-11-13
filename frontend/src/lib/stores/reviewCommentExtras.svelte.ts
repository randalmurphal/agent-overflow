import { createForgeOps, type ForgeOpsHost } from './reviewForgeOps.svelte';
import { createEditorPreviews, type EditorPreviews } from './reviewEditorPreview.svelte';
import { createReviewReactions, type ReviewReactions } from './reviewReactions.svelte';
import { createReviewSuggestions, type ReviewSuggestions } from './reviewSuggestions.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';
import type { ReviewFile } from '../utils/patchStore';

// A review pane's suggested changes, reactions and editor previews
// (docs/specs/pull-requests.md, "Suggestions, reactions and preview"),
// composed for the pane: the one operation runner the reactions and
// applies share, and the slice of the review state they add.

// The backend's suggestions and reactions: the read's fields and the
// writes (ApplyPRSuggestion, SetPRCommentReaction).
const PR_SUGGESTIONS_REACTIONS = 'pr-suggestions-reactions.v1';

/** The members a review pane state carries from here. */
export interface ReviewCommentExtrasState {
  readonly reactions: ReviewReactions;
  readonly suggestions: ReviewSuggestions;
  /** Every editor's Preview toggle, kept with its text. */
  readonly editorPreviews: EditorPreviews;
  /** Why the PR's comments show no reactions, one sentence, or null. */
  readonly reactionsUnavailable: string | null;
}

export interface ReviewCommentExtrasHost extends ForgeOpsHost {
  /** The pane is on a PR in the PR scope. */
  onPR(): boolean;
  /** Why a write to the PR is withheld now (the now writes' reason). */
  withheld(): string | null;
  loadedHeadSHA(): string;
  /** The diff on screen is the PR's whole diff at its live head. */
  diffIsCurrentHead(): boolean;
  files(): readonly ReviewFile[];
  /** The keys of the editors open now; a closed one's preview goes. */
  openEditorKeys(): ReadonlySet<string>;
  reactionsUnavailable(): string | null;
}

export interface ReviewCommentExtras {
  /** Its members keep their getters: merge with Object.defineProperties. */
  readonly state: ReviewCommentExtrasState;
  dispose(): void;
}

export function createReviewCommentExtras(host: ReviewCommentExtrasHost): ReviewCommentExtras {
  const ops = createForgeOps(host);
  const capable = (): boolean => getTransportHelloFor(host.backend)?.capabilities.includes(PR_SUGGESTIONS_REACTIONS) ?? false;
  const reactions = createReviewReactions({ ops, capable, onPR: host.onPR, withheld: host.withheld, unavailable: host.reactionsUnavailable });
  const suggestions = createReviewSuggestions({
    ops,
    capable,
    onPR: host.onPR,
    withheld: host.withheld,
    loadedHeadSHA: host.loadedHeadSHA,
    diffIsCurrentHead: host.diffIsCurrentHead,
    files: host.files,
  });
  const editorPreviews = createEditorPreviews(host.openEditorKeys, host.backend);
  const reactionsUnavailable = $derived(capable() && host.onPR() ? host.reactionsUnavailable() : null);

  return {
    state: {
      reactions,
      suggestions,
      editorPreviews,
      get reactionsUnavailable() { return reactionsUnavailable; },
    },
    dispose() {
      ops.dispose();
      editorPreviews.dispose();
    },
  };
}
