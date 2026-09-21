// `/side-chat`: fork the focused pane's thread into a hidden scratch thread
// and open it in a companion pane beside its source.
//
// The pane owns the fork's lifetime. Closing it (explicitly, with its source,
// or when the source pane changes thread) deletes the thread through
// companionPanes.svelte#closeCompanion; Keep promotes it into an ordinary
// thread and swaps the companion for a normal thread pane in place. Both
// bindings carry the thread id, so the call routes to the computer that owns
// the thread and the pane addresses the same one.

import { ForkSideChat, PromoteScratchThread } from './bindings';
import { closeCompanion, companionForSource, openCompanion } from './companionPanes.svelte';
import { getPaneLayoutItems } from './paneLayout.svelte';
import {
  createPane,
  destroyPane,
  getPane,
  mountThreadInPane,
  openThreadInNewPane,
  syncThread,
} from './panes.svelte';
import { deleteSideChatThread } from './sideChatThread';
import { addToast } from './toast.svelte';
import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import { userFacingError } from '../utils/userFacingError';

export const SIDE_CHAT_COMPANION_KIND = 'side-chat' as const;

export interface SideChatResult {
  /** User-facing failure text. Empty on success. */
  error: string;
}

/**
 * Fork `pane`'s thread into a side chat and open it beside the pane.
 *
 * Available mid-turn: the fork takes the tail with the running turn
 * included, and the source keeps streaming untouched.
 */
export async function openSideChat(pane: ThreadPane): Promise<SideChatResult> {
  const threadId = pane.threadId;
  if (!threadId) return { error: 'Open a thread before starting a side chat.' };
  if (companionForSource(pane.paneId, SIDE_CHAT_COMPANION_KIND)) {
    return { error: 'This pane already has a side chat open.' };
  }

  const companion = openCompanion(pane.paneId, SIDE_CHAT_COMPANION_KIND);
  if (!companion) return { error: 'The pane moved on before the side chat opened.' };
  const preparingPane = createPane(companion.paneId);
  const stillOwnsPane = () => getPane(companion.paneId) === preparingPane;
  let fork: Thread;
  try {
    fork = (await ForkSideChat(threadId)) as Thread;
  } catch (err) {
    if (stillOwnsPane()) closeCompanion(companion.paneId);
    console.error('Failed to start a side chat:', err);
    return { error: userFacingError(err, 'Failed to start a side chat.') };
  }
  // Closing and reopening can reuse the pane ID. Only this particular pane
  // instance may receive the completed fork or be removed on failure.
  if (!stillOwnsPane() || pane.threadId !== threadId) {
    if (stillOwnsPane()) closeCompanion(companion.paneId);
    await deleteSideChatThread(fork.id);
    return { error: 'The pane moved on before the side chat opened.' };
  }
  try {
    await mountThreadInPane(fork, preparingPane);
  } catch (err) {
    console.error('Failed to open the side chat pane:', err);
    if (stillOwnsPane()) closeCompanion(companion.paneId);
    return { error: userFacingError(err, 'Failed to open the side chat.') };
  }
  return { error: '' };
}

/**
 * Keep: promote the side chat's scratch thread into an ordinary thread and
 * replace the companion with a normal thread pane in the same slot.
 */
export async function keepSideChat(paneId: string): Promise<void> {
  const threadId = getPane(paneId)?.threadId;
  if (!threadId) return;
  let promoted: Thread;
  try {
    promoted = (await PromoteScratchThread(threadId)) as Thread;
  } catch (err) {
    console.error('Failed to keep the side chat:', err);
    addToast('error', userFacingError(err, 'Failed to keep this side chat.'));
    return;
  }
  const insertIndex = getPaneLayoutItems().findIndex((item) => item.paneId === paneId);
  // Destroyed rather than closed: the thread is kept, so nothing deletes it.
  // The pane goes first, or the reopen below would find the thread still
  // mounted and reveal the pane that is on its way out. destroyPane cascades
  // into the companion registry through the destroyed-pane observer, which
  // drops the registration.
  destroyPane(paneId);
  syncThread(promoted);
  await openThreadInNewPane(promoted, insertIndex < 0 ? undefined : insertIndex);
}
