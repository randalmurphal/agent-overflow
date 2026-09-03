// Inline review comments: the reaction to `review:comments-changed`. Fan-in
// target of events.ts's setupEventListeners.
//
// Two comment sets ride one channel because they are the same surface twice:
// a proposed plan's inline comments, keyed by plan item, and a diff review's,
// keyed by scope + source key. Both had CRUD that persisted and answered its
// own caller and told nobody, so a comment written on one device never
// appeared on another until that pane reloaded.
//
// The frame carries no comment. A delete is a DELETE-OR-RESOLVE depending on
// whether the comment was already sent, so a row-carrying frame could not say
// what the set now holds; each store re-reads through the same RPC that
// answers its own writes, and only where it already holds that set.

import { resyncDiffReviewComments } from './diffReviewComments.svelte';
import { resyncPlanComments } from './proposedPlans.svelte';
import type { DiffReviewScope } from '../types/models';

/**
 * What the `review:comments-changed` channel carries (internal/app
 * ReviewCommentsChangedEvent): the SET that moved, in the shape of its read
 * RPC's arguments. Exactly one of the two forms is filled.
 */
export interface ReviewCommentsChangedEvent {
  threadId: string;
  planItemId?: string;
  scope?: DiffReviewScope;
  sourceKey?: string;
}

export function applyReviewCommentsChanged(evt: ReviewCommentsChangedEvent): void {
  const threadId = evt?.threadId;
  if (!threadId) return;
  if (evt.planItemId) {
    void resyncPlanComments(threadId, evt.planItemId).catch((err) => {
      console.error('review comments: re-read plan comments failed:', err);
    });
    return;
  }
  if (evt.scope && evt.sourceKey) {
    void resyncDiffReviewComments(threadId, evt.scope, evt.sourceKey).catch((err) => {
      console.error('review comments: re-read diff review comments failed:', err);
    });
  }
}
