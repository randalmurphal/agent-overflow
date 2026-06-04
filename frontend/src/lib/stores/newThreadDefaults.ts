import type { ThreadPane } from './thread.svelte';
import {
  UpdateNewThreadDefaults,
  type NewThreadDefaultsUpdateInput,
  type ThreadDefaults,
} from './bindings';

export async function updatePlaceholderDefaults(
  pane: ThreadPane,
  update: Omit<NewThreadDefaultsUpdateInput, 'projectId'>,
): Promise<ThreadDefaults | null> {
  const thread = pane.thread;
  if (!thread?.projectId) return null;
  const placeholderId = pane.hasDraftPlaceholder ? thread.id : null;
  const defaults = await UpdateNewThreadDefaults({
    projectId: thread.projectId,
    provider: update.provider ?? thread.provider,
    model: update.model ?? thread.model,
    ...update,
  });
  if (placeholderId && pane.hasDraftPlaceholder && pane.thread?.id === placeholderId) {
    pane.applyDraftPlaceholderDefaults(defaults);
  }
  return defaults;
}
