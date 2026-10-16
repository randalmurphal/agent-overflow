// New Assistant (docs/specs/standing-agents.md, "Sidebar"): every entry
// point opens one dialog with the project it came from preselected; the
// dialog creates the assistant and opens its thread where the action asked.

import type { ThreadPane } from './thread.svelte';

export interface NewAssistantRequest {
  projectId: string;
  /** The pane the action came from; the new thread opens there. */
  targetPane?: ThreadPane | null;
  /** The new-thread modifier was held: open in a new pane instead. */
  openInNewPane?: boolean;
}

let pending: NewAssistantRequest | null = $state(null);

export function getNewAssistantRequest(): NewAssistantRequest | null {
  return pending;
}

export function openNewAssistantDialog(request: NewAssistantRequest): void {
  pending = request;
}

export function closeNewAssistantDialog(): void {
  pending = null;
}
