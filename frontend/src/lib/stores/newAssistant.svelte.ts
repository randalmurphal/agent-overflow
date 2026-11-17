// New Assistant (docs/specs/standing-agents.md, "Sidebar"): every entry
// point opens one dialog, with the project it came from preselected when
// it came from one; the dialog creates the assistant on that project's
// computer, or the selected one, and opens its thread where the action
// asked. The assistant's Projects dialog changes what it serves; it is
// held by the assistant's id, so a handoff while it is open moves it to
// the successor.
//
// Each opening of either dialog is its own object. A write the dialog
// started may answer after Cancel or a later opening: its result still
// reaches the catalog, but only the opening that started it may close,
// show an error, or open the thread it made.

import type { BackendKey } from '../transport/backendKey';
import type { Thread } from '../types/models';
import { selectedBackend } from './selectedBackend.svelte';
import { assistantsOn, threadComputer } from './assistants.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { projectBackend } from '../transport/entityIndex';
import type { ThreadPane } from './thread.svelte';

export interface NewAssistantRequest {
  /** The project the action came from, preselected; none from the Assistants header. */
  projectId?: string;
  /** The pane the action came from; the new thread opens there. */
  targetPane?: ThreadPane | null;
  /** The new-thread modifier was held: open in a new pane instead. */
  openInNewPane?: boolean;
}

/** One opening of New Assistant. */
export interface NewAssistantOpening extends Readonly<NewAssistantRequest> {
  /**
   * The computer selected when it opened, which an opening from no project
   * creates on; a later change of selection does not move it.
   */
  readonly selectedBackend: BackendKey | undefined;
}

/** One opening of an assistant's Projects dialog. */
export interface AssistantProjectsOpening {
  readonly agentId: string;
}

// Raw: an opening's identity is what a late write compares.
let pending: NewAssistantOpening | null = $state.raw(null);
let projectsOf: AssistantProjectsOpening | null = $state.raw(null);

export function getNewAssistantRequest(): NewAssistantOpening | null {
  return pending;
}

/**
 * Opens New Assistant; nothing while the computer it would create on, the
 * project's or the selected one, has assistants turned off.
 */
export function openNewAssistantDialog(request: NewAssistantRequest): void {
  const selected = request.projectId ? undefined : selectedBackend();
  const target = request.projectId ? projectBackend(request.projectId) : selected;
  if (!assistantsOn(target ?? HOME_BACKEND)) return;
  pending = { ...request, selectedBackend: selected };
}

/** Whether `opening` is the dialog's current opening, so a late answer may still act on it. */
export function isNewAssistantOpening(opening: NewAssistantOpening): boolean {
  return pending === opening;
}

/** Closes the dialog; given an opening, only while it is still the current one. */
export function closeNewAssistantDialog(opening?: NewAssistantOpening): void {
  if (opening === undefined || pending === opening) pending = null;
}

/** The Projects dialog's current opening, or null. */
export function getAssistantProjectsOpening(): AssistantProjectsOpening | null {
  return projectsOf;
}

/** The id of the assistant whose Projects dialog is open, or null. */
export function getAssistantProjectsAgentId(): string | null {
  return projectsOf?.agentId ?? null;
}

/**
 * Opens the Projects dialog of the assistant whose current thread this is;
 * any other thread has none, and nothing opens while its computer has
 * assistants off.
 */
export function openAssistantProjectsDialog(thread: Pick<Thread, 'id' | 'agentId'>): void {
  if (thread.agentId && assistantsOn(threadComputer(thread.id))) projectsOf = { agentId: thread.agentId };
}

/** Whether `opening` is the Projects dialog's current opening. */
export function isAssistantProjectsOpening(opening: AssistantProjectsOpening): boolean {
  return projectsOf === opening;
}

/** Closes the Projects dialog; given an opening, only while it is still the current one. */
export function closeAssistantProjectsDialog(opening?: AssistantProjectsOpening): void {
  if (opening === undefined || projectsOf === opening) projectsOf = null;
}
