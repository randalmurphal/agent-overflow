import { GetThreadDefaults } from './bindings';
import { getProject, getProjects } from './projects.svelte';
import {
  ensureMainPane,
  ensurePaneInLayout,
  getFocusedPaneOrNull,
  openEmptyPane,
} from './panes.svelte';
import { expandProject } from './sidebar.svelte';
import type { DraftPlaceholderDefaults, ThreadPane } from './thread.svelte';

export type DraftMode = 'chat' | 'design';

/**
 * Resolve which project the next draft thread should land in when the
 * source of the request doesn't supply one (e.g. the global Ctrl+N
 * keybinding firing without a focused thread). Prefers the focused
 * pane's current project, then falls back to the most recently active
 * project (ListProjects is sorted server-side by lastActive
 * descending). Returns null when no projects exist at all — the
 * caller should surface "add a project first".
 */
export function resolveDraftTargetProject(
  targetPane: ThreadPane | null,
): { projectId: string; mode: DraftMode } | null {
  const fromPane = targetPane?.thread?.projectId;
  if (fromPane) {
    return { projectId: fromPane, mode: targetPane!.activeTab };
  }
  const fallback = getProjects()[0]?.project.id;
  if (!fallback) return null;
  return { projectId: fallback, mode: targetPane?.activeTab ?? 'chat' };
}

export interface OpenDraftThreadOptions {
  projectId: string;
  mode: DraftMode;
  targetPane?: ThreadPane | null;
  openInNewPane?: boolean;
}

/**
 * Open a fresh in-pane draft placeholder for a project. The placeholder is
 * a pure UI state — no SQLite row is written. The first composer input
 * (typed text, paste, attachment upload) or toolbar action calls
 * `pane.ensureMaterializedThread()`, which creates the backend row,
 * prepends it to the sidebar with `isDraft=true`, and points the
 * composer-draft store at the new id. "+ New" repeated without any
 * action simply replaces the prior placeholder, so the user can spin up
 * and discard threads freely.
 *
 * Returns the pane the placeholder was opened in so callers can layer
 * additional state (e.g. seed-from-plan flows) on top.
 */
export async function openDraftThreadForProject(
  options: OpenDraftThreadOptions,
): Promise<ThreadPane> {
  const { projectId, mode, targetPane, openInNewPane = false } = options;
  expandProject(projectId);
  const project = getProject(projectId)?.project;
  if (!project) {
    throw new Error('Project not found');
  }
  const pane: ThreadPane = openInNewPane
    ? openEmptyPane()
    : (targetPane ?? getFocusedPaneOrNull() ?? ensureMainPane());
  // The placeholder is in-memory only — it doesn't go through
  // openThreadInPane, so we need to make sure the pane is mounted in
  // the layout grid ourselves. openEmptyPane already attaches itself.
  ensurePaneInLayout(pane.paneId);
  // Fetch the same seed values CreateThread would have used (last-used
  // model profile + current git branch) so the placeholder's toolbar
  // and workspace strip don't render "no model / no branch" before
  // materialization. Failure here is tolerable — we still want the
  // placeholder to appear; the user can pick from the toolbar.
  let defaults: DraftPlaceholderDefaults | undefined;
  try {
    defaults = await GetThreadDefaults({ projectId, mode });
  } catch (err) {
    console.warn('GetThreadDefaults failed; using empty placeholder defaults', err);
  }
  pane.startDraftPlaceholder(project, mode, defaults);
  return pane;
}
