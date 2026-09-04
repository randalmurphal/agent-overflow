// The one implementation of "flip this thread between build and plan",
// shared by the toolbar's mode button and the minimal rung's roll-up row
// so the two cannot drift on the draft-placeholder branch.
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import { UpdateThreadMode } from '../../../stores/bindings';
import { syncThread } from '../../../stores/panes.svelte';
import { addToast } from '../../../stores/toast.svelte';
import { cycleMode, type CycleMode } from '../../../utils/modeCycle';
import { errString } from '../../../utils/errors';

/** The thread's mode as the toggle presents it; an unset mode reads as chat. */
export function currentAgentMode(pane: ThreadPane): CycleMode {
  return (pane.thread?.mode as CycleMode | undefined) ?? 'chat';
}

/**
 * Advance the pane's thread to the next agent mode. A draft placeholder
 * changes locally; a persisted thread goes through the RPC and syncs the
 * row it answers with. Failures surface as a toast.
 */
export async function cycleAgentMode(pane: ThreadPane): Promise<void> {
  if (!pane.thread) return;
  const next = cycleMode(currentAgentMode(pane));
  if (pane.hasDraftPlaceholder) {
    pane.setDraftPlaceholderMode(next);
    return;
  }
  const threadId = pane.threadId;
  if (!threadId) return;
  try {
    const updated = (await UpdateThreadMode(threadId, next)) as Thread;
    syncThread(updated);
  } catch (err) {
    console.error('agent mode toggle: UpdateThreadMode failed', err);
    addToast('error', `Failed to switch mode: ${errString(err)}`);
  }
}
