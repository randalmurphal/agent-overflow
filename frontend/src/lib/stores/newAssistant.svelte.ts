// New Assistant (docs/specs/standing-agents.md, "Sidebar"): every entry
// point opens one dialog, with the project it came from preselected when
// it came from one; the dialog creates the assistant on that project's
// computer, or the selected one, and opens its thread where the action
// asked. The assistant's Projects dialog changes what it serves; it is
// held by the assistant's id, so a handoff while it is open moves it to
// the successor.

import type { Thread } from '../types/models';
import type { ThreadPane } from './thread.svelte';

export interface NewAssistantRequest {
  /** The project the action came from, preselected; none from the Assistants header. */
  projectId?: string;
  /** The pane the action came from; the new thread opens there. */
  targetPane?: ThreadPane | null;
  /** The new-thread modifier was held: open in a new pane instead. */
  openInNewPane?: boolean;
}

let pending: NewAssistantRequest | null = $state(null);
let projectsOf: string | null = $state(null);

export function getNewAssistantRequest(): NewAssistantRequest | null {
  return pending;
}

export function openNewAssistantDialog(request: NewAssistantRequest): void {
  pending = request;
}

export function closeNewAssistantDialog(): void {
  pending = null;
}

/** The id of the assistant whose Projects dialog is open, or null. */
export function getAssistantProjectsAgentId(): string | null {
  return projectsOf;
}

/** Opens the Projects dialog of the assistant whose current thread this is; any other thread has none. */
export function openAssistantProjectsDialog(thread: Pick<Thread, 'agentId'>): void {
  if (thread.agentId) projectsOf = thread.agentId;
}

export function closeAssistantProjectsDialog(): void {
  projectsOf = null;
}
