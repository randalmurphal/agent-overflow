import type { ThreadPane } from './thread.svelte';
import { forEachDraftPlaceholderPane } from './panes.svelte';
import {
  UpdateNewThreadDefaults,
  type NewThreadDefaultsUpdateInput,
  type ThreadDefaults,
} from './bindings';

/**
 * Write the project's new-thread defaults and reflect them in every open
 * draft-placeholder composer on that project.
 *
 * The defaults are a PROJECT fact, not the acting pane's: the backend stores
 * one row per project and the next thread created in any pane picks it up.
 * Applying the result only to the pane that was clicked left every other
 * "New Thread" composer on the project showing — and about to create a thread
 * with — the superseded model / effort / mode.
 */
export async function updatePlaceholderDefaults(
  pane: ThreadPane,
  update: Omit<NewThreadDefaultsUpdateInput, 'projectId'>,
): Promise<ThreadDefaults | null> {
  const thread = pane.thread;
  const projectId = thread?.projectId;
  if (!thread || !projectId) return null;
  const defaults = await UpdateNewThreadDefaults({
    projectId,
    provider: update.provider ?? thread.provider,
    model: update.model ?? thread.model,
    ...update,
  });
  const reached = new Set<string>();
  forEachDraftPlaceholderPane(projectId, (target) => {
    reached.add(target.paneId);
    target.applyDraftPlaceholderDefaults(defaults);
  });
  // The acting pane is normally one of those; apply directly when it is not
  // in the registry (component tests build panes standalone). Re-checked
  // against the project because the pane may have switched under the await,
  // and applyDraftPlaceholderDefaults itself no-ops once the placeholder is
  // gone — together that is the old placeholder-identity guard.
  if (!reached.has(pane.paneId) && pane.thread?.projectId === projectId) {
    pane.applyDraftPlaceholderDefaults(defaults);
  }
  return defaults;
}
