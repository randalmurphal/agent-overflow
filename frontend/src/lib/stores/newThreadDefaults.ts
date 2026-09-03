import type { ThreadPane } from './thread.svelte';
import { forEachDraftPlaceholderPane } from './panes.svelte';
import {
  UpdateNewThreadDefaults,
  type NewThreadDefaultsUpdateInput,
  type ThreadDefaults,
} from './bindings';

/**
 * Payload of `chatbar:new-thread-defaults` (Go: app.NewThreadDefaultsChangedEvent).
 * The seed a future thread gets, and the project whose open draft placeholders
 * adopt it.
 */
export interface NewThreadDefaultsChangedEvent {
  projectId: string;
  defaults: ThreadDefaults;
}

/**
 * Write the new-thread defaults and reflect them in every open
 * draft-placeholder composer on the acting pane's project.
 *
 * The defaults are not the acting pane's: the next thread created in ANY pane
 * on that project picks them up. Applying the result only to the pane that was
 * clicked left every other "New Thread" composer on the project showing — and
 * about to create a thread with — the superseded model / effort / mode.
 *
 * The persisted row is keyed by provider and model, app-wide, but the SET a
 * write converges is deliberately the project's: choosing a model in one
 * project's composer is not a statement about another's open placeholder. That
 * choice is why the backend's `chatbar:new-thread-defaults` frame carries the
 * project id, and why `applyNewThreadDefaults` below is this same fan-out.
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

/**
 * The same fan-out, driven by another client's write.
 *
 * Before this, `UpdateNewThreadDefaults` answered its caller and told nobody:
 * a second device's placeholder toolbar kept the superseded model, effort and
 * runtime mode, and would have created a thread with them. The frame carries
 * exactly the pair the writer applies above, so the writer's own echo repeats
 * an apply it already made and `applyDraftPlaceholderDefaults` no-ops on a
 * pane whose placeholder is gone.
 */
export function applyNewThreadDefaults(evt: NewThreadDefaultsChangedEvent): void {
  const projectId = evt?.projectId;
  const defaults = evt?.defaults;
  if (!projectId || !defaults) return;
  forEachDraftPlaceholderPane(projectId, (target) => {
    target.applyDraftPlaceholderDefaults(defaults);
  });
}
