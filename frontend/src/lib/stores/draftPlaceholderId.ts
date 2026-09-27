// The id of a draft placeholder thread (./threadDraftPlaceholder.svelte.ts):
// `draft:<paneId>:<projectId>:<mode>:<random>`. A placeholder has no thread
// row, so the transport resolves its owner through the project its id names
// (../transport/threadOwner.ts). The backend recognizes the `draft:` prefix
// (internal/app/app_terminal.go).

import { randomId } from '../utils/randomId';
import type { DraftPlaceholderMode } from './threadPaneShared';

const PREFIX = 'draft:';

/** A fresh placeholder id for a draft of `projectId` in pane `paneId`. */
export function draftPlaceholderId(paneId: string, projectId: string, mode: DraftPlaceholderMode): string {
  return `${PREFIX}${paneId}:${projectId}:${mode}:${randomId()}`;
}

/** Whether `threadId` is a draft placeholder's id rather than a thread's. */
export function isDraftPlaceholderId(threadId: string): boolean {
  return threadId.startsWith(PREFIX);
}

/**
 * The project a placeholder id names, or undefined for any other id.
 * Parsed from the right: a pane id may contain `:`, while a project id
 * (internal/entityid), a mode and the random suffix do not.
 */
export function draftPlaceholderProjectId(threadId: string): string | undefined {
  if (!threadId.startsWith(PREFIX)) return undefined;
  const randomSep = threadId.lastIndexOf(':');
  const modeSep = threadId.lastIndexOf(':', randomSep - 1);
  const projectSep = threadId.lastIndexOf(':', modeSep - 1);
  // The prefix's own `:` is not a separator: the pane id segment is missing.
  if (projectSep < PREFIX.length) return undefined;
  return threadId.slice(projectSep + 1, modeSep);
}
