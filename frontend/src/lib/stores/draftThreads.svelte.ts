// Tracks the in-flight "draft" thread per (project, mode) pair. When the
// user clicks "+ New" on a project we create the thread row on the
// backend (for provider defaults + stable id) but keep it out of the
// sidebar and keep its provider process unspawned until the first
// SendMessage. Clicking "+ New" again for the same project + mode
// returns the same draft so the composerDraft cache (persisted backend-
// side by threadId) repopulates the composer.
//
// Mode keying: + New is mode-contextual — clicking + with the Design tab
// active creates a design thread; with the Chat tab active it creates a
// chat thread. Each (project, mode) gets its own draft slot so both
// drafts can coexist (user can have a fresh chat thread AND a fresh
// design thread queued up in the same project).
//
// Map shape: `${projectId}|${mode}` → draft Thread, where mode is the
// thread's persisted mode column ('chat' | 'design'). Plan threads are a
// sub-mode of chat that emerges only after creation; drafts created via
// + always start as 'chat' or 'design'.
//
// Persistence: none. A crash / reload loses the draft pointer; the
// underlying thread row still lives in the DB but is hidden by
// ListThreadsWithItems. Acceptable for now — orphan empty rows can be
// GC'd in a follow-up if they accumulate.

import type { Thread } from '../types/models';

/** The set of modes that get their own draft slot. */
export type DraftMode = 'chat' | 'design';

type DraftKey = `${string}|${DraftMode}`;

function key(projectId: string, mode: DraftMode): DraftKey {
  return `${projectId}|${mode}` as DraftKey;
}

let drafts: Map<DraftKey, Thread> = $state(new Map());

/** Returns the current draft thread for a (project, mode), or undefined. */
export function getProjectDraft(
  projectId: string,
  mode: DraftMode = 'chat',
): Thread | undefined {
  return drafts.get(key(projectId, mode));
}

/** Replace the draft thread for a (project, mode). */
export function setProjectDraft(
  projectId: string,
  mode: DraftMode,
  thread: Thread,
): void {
  drafts = new Map(drafts).set(key(projectId, mode), thread);
}

/**
 * Drop the draft pointer for a (project, mode). Called on successful
 * first SendMessage (the thread is no longer a draft — it's in the
 * sidebar now) or when the thread is deleted externally.
 */
export function clearProjectDraft(projectId: string, mode: DraftMode): void {
  const k = key(projectId, mode);
  if (!drafts.has(k)) return;
  const next = new Map(drafts);
  next.delete(k);
  drafts = next;
}

/**
 * Reverse lookup: find the (projectId, mode) pair a given threadId is
 * drafting for. Used by Composer.send() to promote-and-clear on first
 * successful send when the pane only knows the threadId.
 */
export function findDraftEntry(
  threadId: string,
): { projectId: string; mode: DraftMode } | undefined {
  for (const [k, thread] of drafts) {
    if (thread.id !== threadId) continue;
    const sep = k.lastIndexOf('|');
    if (sep < 0) continue;
    return {
      projectId: k.slice(0, sep),
      mode: k.slice(sep + 1) as DraftMode,
    };
  }
  return undefined;
}

/**
 * Read-only view of the full draft map. Test helper.
 */
export function getAllDrafts(): Map<DraftKey, Thread> {
  return drafts;
}

/** Wipe all draft pointers. Test isolation only. */
export function resetForTest(): void {
  if (drafts.size === 0) return;
  drafts = new Map();
}
