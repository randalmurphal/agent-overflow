// The pane title's rename affordance, reachable from outside the header.
//
// Two halves, one owner. `PaneTitleHandle` publishes an imperative handle so a
// command elsewhere (the composer's `/rename` with no argument) can open the
// same inline editor the user gets by right-clicking the title — rather than a
// second rename UI that would have to be kept in step with it. And
// `renameThreadTitle` is the write itself, shared so `/rename <text>` and the
// inline editor's Enter commit are literally the same call.
//
// Registry shape mirrors composerPickerRegistry: a plain Map keyed by paneId,
// no `$state`, because callers invoke it imperatively and never subscribe.

import { GetThread, RenameThread } from './bindings';
import { syncThread } from './panes.svelte';
import type { Thread } from '../types/models';
import { errString } from '../utils/errors';

export interface PaneTitleRenameHandle {
  /** Opens the inline editor seeded with the current title. */
  start: () => void;
}

const handles = new Map<string, PaneTitleRenameHandle>();

export function registerPaneTitleRename(
  paneId: string,
  handle: PaneTitleRenameHandle,
): () => void {
  handles.set(paneId, handle);
  return () => {
    if (handles.get(paneId) === handle) handles.delete(paneId);
  };
}

/** Open the pane's inline rename editor. False when the pane has no title. */
export function startPaneTitleRename(paneId: string | null | undefined): boolean {
  if (!paneId) return false;
  const handle = handles.get(paneId);
  if (!handle) return false;
  handle.start();
  return true;
}

export interface RenameResult {
  ok: boolean;
  /** User-facing failure text, already formatted. */
  error?: string;
}

/**
 * Persist a new title and refresh the row.
 *
 * `RenameThread` returns void, so the row is re-read rather than
 * hand-assembled — the sidebar and every pane showing the thread pick the new
 * title up from one `syncThread`. An empty or unchanged title is a quiet
 * no-op: the user is already looking at that title, so there is nothing to
 * report.
 */
export async function renameThreadTitle(
  threadId: string,
  title: string,
  currentTitle?: string,
): Promise<RenameResult> {
  const next = title.trim();
  if (next === '' || next === currentTitle) return { ok: true };
  try {
    await RenameThread(threadId, next);
    const updated = (await GetThread(threadId)) as Thread;
    syncThread(updated);
    return { ok: true };
  } catch (err) {
    console.error('Rename thread failed:', err);
    return { ok: false, error: `Failed to rename thread: ${errString(err)}` };
  }
}

export function resetPaneTitleRenameForTest(): void {
  handles.clear();
}
