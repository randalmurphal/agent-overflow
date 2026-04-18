// Tracks the in-flight "draft" thread per project. When the user clicks
// "New Thread" we create the thread row on the backend (for provider
// defaults + stable id) but keep it out of the sidebar and keep its
// provider process unspawned until the first SendMessage. Clicking
// "New Thread" again for the same project while the draft hasn't been
// sent returns the same draft so the composerDraft cache (persisted
// backend-side by threadId) repopulates the composer.
//
// Map shape: projectId → draft Thread. We cache the full Thread so the
// pane can switchThread without a GetThread round-trip. When a draft is
// sent, Composer.send() promotes it to the sidebar via prependThread
// and clears the entry here.
//
// Persistence: none. A crash / reload loses the draft pointer; the
// underlying thread row still lives in the DB but is hidden by
// ListThreadsWithItems. Acceptable for now — orphan empty rows can be
// GC'd in a follow-up if they accumulate.

import type { Thread } from '../types/models';

let drafts: Map<string, Thread> = $state(new Map());

/** Returns the current draft thread for a project, or undefined. */
export function getProjectDraft(projectId: string): Thread | undefined {
  return drafts.get(projectId);
}

/** Replace the draft thread for a project. */
export function setProjectDraft(projectId: string, thread: Thread): void {
  drafts = new Map(drafts).set(projectId, thread);
}

/**
 * Drop the draft pointer for a project. Called on successful first
 * SendMessage (the thread is no longer a draft — it's in the sidebar now)
 * or when the thread is deleted externally.
 */
export function clearProjectDraft(projectId: string): void {
  if (!drafts.has(projectId)) return;
  const next = new Map(drafts);
  next.delete(projectId);
  drafts = next;
}

/**
 * Reverse lookup: find the project a given threadId is drafting for. Used
 * by Composer.send() to promote-and-clear on first successful send when
 * the pane only knows the threadId, not the projectId.
 */
export function findDraftProjectId(threadId: string): string | undefined {
  for (const [projectId, thread] of drafts) {
    if (thread.id === threadId) return projectId;
  }
  return undefined;
}

/**
 * Read-only view of the full draft map. Test helper.
 */
export function getAllDrafts(): Map<string, Thread> {
  return drafts;
}

/** Wipe all draft pointers. Test isolation only. */
export function resetForTest(): void {
  if (drafts.size === 0) return;
  drafts = new Map();
}
