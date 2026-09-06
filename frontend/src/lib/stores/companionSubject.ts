import type { Thread } from '../types/models';
import type { WorkspaceRef } from '../types/git';

/** A conversation can keep its ID while its owner or checkout changes. Both
 * component mounts and captured review state must retire together at that edge. */
export function companionSubjectKey(pane: {
  thread: Thread | null;
  workspace: WorkspaceRef | null;
}): string {
  const thread = pane.thread;
  if (!thread) return '';
  return JSON.stringify([
    thread.id, thread.ownershipEpoch ?? 0,
    pane.workspace?.projectId ?? thread.projectId ?? '',
    pane.workspace?.workspacePath ?? thread.workspacePath ?? '',
  ]);
}
