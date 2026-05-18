import { CreateThread } from './bindings';
import { getProject } from './projects.svelte';
import {
  getProjectDraft,
  setProjectDraft,
  type DraftMode,
} from './draftThreads.svelte';
import {
  openThreadInNewPane,
  openThreadInPane,
} from './panes.svelte';
import { expandProject } from './sidebar.svelte';
import type { ThreadPane } from './thread.svelte';
import { seedDefaultWorktreeIntentForDraft } from './worktreeIntent.svelte';
import type { Thread } from '../types/models';

export interface OpenDraftThreadOptions {
  projectId: string;
  mode: DraftMode;
  targetPane?: ThreadPane | null;
  openInNewPane?: boolean;
}

export async function openDraftThreadForProject(options: OpenDraftThreadOptions): Promise<Thread> {
  const { projectId, mode, targetPane, openInNewPane = false } = options;
  expandProject(projectId);
  const existing = getProjectDraft(projectId, mode);
  if (existing) {
    if (openInNewPane) await openThreadInNewPane(existing);
    else if (targetPane) await openThreadInPane(existing, targetPane);
    else await openThreadInPane(existing);
    return existing;
  }
  const project = getProject(projectId)?.project;
  if (!project) {
    throw new Error('Project not found');
  }
  const created = await CreateThread({
    projectId,
    mode: mode === 'design' ? 'design' : 'chat',
  });
  setProjectDraft(projectId, mode, created);
  seedDefaultWorktreeIntentForDraft(created);
  if (openInNewPane) await openThreadInNewPane(created);
  else if (targetPane) await openThreadInPane(created, targetPane);
  else await openThreadInPane(created);
  return created;
}
