// stores/threadPaneCompanions.ts
//
// OWNS the pane's side of its companion surfaces — plan sidebar, review
// pane and the agent companion: which of them is open,
// what opening each one actually means (the review pane needs a review
// SUBJECT — the checkout, plus the thread row when there is one — and an
// async open; closing the
// agent pane drops its breadcrumb trail), the stash a pane leaving its
// thread runs, and the close-them-all sweep a pane clear runs.
//
// MUST NOT hold companion STATE. `companionPanes.svelte.ts` is the registry
// and the only owner of what is mounted where; this module is one pane's
// vocabulary over it, so every read goes back to that registry rather than
// caching a local flag that could disagree with the layout.
// The agent-pane RETENTION rules (which rows an open scope pins) are a
// timeline concern and stay on the pane.

import type { Thread } from '../types/models';
import {
  closeCompanion,
  closeCompanionsForSource,
  companionForSource,
  isCompanionOpen,
  openCompanion,
  toggleCompanion,
} from './companionPanes.svelte';
import { openReviewCompanion, type ReviewSubject } from './reviewPane.svelte';
import { disposeAgentStateForPane, openAgentCompanion } from './agentPane.svelte';
import { stashCompanions } from './companionStash';

export interface ThreadPaneCompanionsOptions {
  paneId: string;
  getThread(): Thread | null;
  /** The review pane's subject, read lazily from the composition root
   *  (`reviewSubjectForPane`). A draft placeholder has one — a checkout with
   *  no thread row yet — so this is not `getThread()?.id`. */
  getReviewSubject(): ReviewSubject | null;
}

export function createThreadPaneCompanions(options: ThreadPaneCompanionsOptions) {
  const { paneId } = options;

  function closeFor(kind: 'plan' | 'review' | 'agent'): void {
    const companion = companionForSource(paneId, kind);
    if (companion) closeCompanion(companion.paneId);
  }

  return {
    get showPlanSidebar() {
      return isCompanionOpen(paneId, 'plan');
    },
    get showReviewPane() {
      return isCompanionOpen(paneId, 'review');
    },
    /** Whether the agent companion (a subagent's scoped thread view) is
     *  open for this pane. There is at most one — opening another node
     *  swaps its scope (docs/specs/agent-visibility.md Q4b). */
    get showAgentPane() {
      return isCompanionOpen(paneId, 'agent');
    },

    /**
     * The pane is leaving its thread (closing, or "+ New" on it): hide its
     * companions and remember them for that thread, which reopens them
     * when it is shown again (companionStash.ts).
     */
    stashAll(): void {
      stashCompanions(paneId, options.getThread());
    },

    /**
     * Companions are per-thread surfaces; an emptied pane keeps none.
     * Every leaving-the-thread edge has stashed them by now, so this closes
     * only what a clear without leaving holds: registry resets and a
     * deleted thread's pane.
     */
    closeAll(): void {
      closeCompanionsForSource(paneId);
    },

    togglePlanSidebar(): void {
      toggleCompanion(paneId, 'plan');
    },

    setShowPlanSidebar(value: boolean): void {
      if (value) openCompanion(paneId, 'plan');
      else closeFor('plan');
    },

    toggleReviewPane(): void {
      const companion = companionForSource(paneId, 'review');
      if (companion) {
        closeCompanion(companion.paneId);
        return;
      }
      const subject = options.getReviewSubject();
      if (subject) void openReviewCompanion(paneId, subject);
    },

    setShowReviewPane(value: boolean): void {
      if (value) {
        const subject = options.getReviewSubject();
        if (subject) void openReviewCompanion(paneId, subject);
      }
      else closeFor('review');
    },

    /**
     * Open the agent companion scoped to `launchItemId` (an Agent/Task row,
     * a forked Skill, a Codex spawn_agent — whatever the card was for), or
     * re-scope the one already open. Opened from CARDS only; there is no
     * header button, because "which agent" is not a question a header can
     * answer.
     */
    openAgentPane(launchItemId: string, label: string): void {
      const threadId = options.getThread()?.id;
      if (!threadId) return;
      openAgentCompanion(paneId, threadId, launchItemId, label);
    },

    closeAgentPane(): void {
      closeFor('agent');
      // Explicitly closing drops the trail: the next open arrives with its
      // own launch row, so a retained breadcrumb could only be stale.
      disposeAgentStateForPane(paneId);
    },
  };
}

export type ThreadPaneCompanions = ReturnType<typeof createThreadPaneCompanions>;
