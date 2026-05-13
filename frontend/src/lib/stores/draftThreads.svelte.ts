// Tracks materialized draft threads per (project, mode) pair. Clicking
// "+ New" starts as a local pane placeholder; the backend row is created
// only after the user adds state worth preserving (text, attachments,
// terminal chips, or seeded plan context). Once materialized, this store
// lets "+ New" return to that draft so the composerDraft cache (persisted
// backend-side by threadId) repopulates the composer.
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
// Persistence: none. A crash / reload loses the pointer; non-empty draft
// rows still live in SQLite and are returned by ListThreadsWithItems.

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
