// The agent thread companion (docs/specs/standing-agents.md, "Agent thread
// companion"): an Agents row's thread opens in one companion pane beside the
// pane whose tray it came from, an ordinary thread view with its own
// composer and Stop. Another row swaps the thread in the same pane. Closing
// it never deletes the thread; Open as Pane swaps it for a normal pane in
// the same slot.

import { closeCompanion, companionForSource, openCompanion } from './companionPanes.svelte';
import { getPaneLayoutItems } from './paneLayout.svelte';
import {
  createPane,
  destroyPane,
  findPaneShowingThread,
  focusPane,
  getPane,
  mountThreadInPane,
  openThreadInNewPane,
} from './panes.svelte';
import { addErrorToast } from './toast.svelte';
import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import { userFacingError } from '../utils/userFacingError';

export const AGENT_THREAD_COMPANION_KIND = 'agent-thread' as const;

/**
 * Show `thread` in `source`'s agent thread companion, opening the companion
 * when there is none. A thread already shown in some pane is revealed there
 * instead, since a thread is in one pane at a time.
 */
export async function openAgentThreadCompanion(source: ThreadPane, thread: Thread): Promise<void> {
  const shown = findPaneShowingThread(thread.id);
  if (shown) {
    focusPane(shown.paneId);
    return;
  }
  const existing = companionForSource(source.paneId, AGENT_THREAD_COMPANION_KIND);
  const companion = existing ?? openCompanion(source.paneId, AGENT_THREAD_COMPANION_KIND);
  if (!companion) throw new Error('The pane moved on before the thread opened.');
  const pane = getPane(companion.paneId) ?? createPane(companion.paneId);
  try {
    await mountThreadInPane(thread, pane, 'committed', { inPlace: true });
  } catch (err) {
    // A companion this call opened leaves with the failed mount; one that
    // was already showing a thread keeps it.
    if (!existing && getPane(companion.paneId) === pane) closeCompanion(companion.paneId);
    throw err;
  }
}

/**
 * Open as Pane: the companion's thread moves to a normal thread pane in the
 * companion's slot, which keeps it after its source leaves.
 */
export async function openAgentThreadCompanionAsPane(paneId: string): Promise<void> {
  const thread = getPane(paneId)?.thread;
  if (!thread) return;
  const insertIndex = getPaneLayoutItems().findIndex((item) => item.paneId === paneId);
  // The pane goes first, or the open below would find the thread still
  // mounted and reveal the pane on its way out. destroyPane drops the
  // companion registration through the destroyed-pane observer, and its
  // close edge stashes the companion's own companions under the thread, so
  // the new pane reopens them.
  destroyPane(paneId);
  try {
    await openThreadInNewPane(thread, insertIndex < 0 ? undefined : insertIndex);
  } catch (err) {
    console.error('Failed to open the agent thread as a pane:', err);
    addErrorToast(userFacingError(err, `Failed to open ${thread.title || 'the thread'} as a pane.`), err);
  }
}
