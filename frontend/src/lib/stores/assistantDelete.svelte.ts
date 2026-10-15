// Deleting an assistant's current thread deletes the assistant and removes its
// home folder (docs/specs/standing-agents.md, "Deletion, archive and
// transfer"). Every confirmation of such a delete names the folder, and the
// delete is always confirmed, whatever the confirmDelete setting says.

import { GetStandingAgent } from './bindings';
import { getSettings } from './settings.svelte';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';

/** Whether deleting these threads deletes an assistant. */
export function deletesAssistant(threads: readonly Thread[]): boolean {
  return threads.some((thread) => Boolean(thread.agentId));
}

/** Whether deleting these threads is confirmed first. */
export function confirmsThreadDelete(threads: readonly Thread[]): boolean {
  return getSettings().confirmDelete || deletesAssistant(threads);
}

/** The dialog title for deleting one thread. */
export function threadDeleteTitle(thread: Thread): string {
  return thread.agentId ? 'Delete Assistant' : 'Delete Thread';
}

/**
 * The sentence for one assistant a delete removes. Until its record is read
 * the folder is not named; the dialog shows the named sentence once it is.
 */
export function assistantDeleteSentence(name: string, home: string | undefined): string {
  const folder = home ? `its home folder ${home}` : 'its home folder';
  return `The assistant "${name}" is deleted too, and ${folder} is removed with everything in it.`;
}

/** The sentences for the assistants a project's delete removes, or ''. */
export function projectAssistantsNote(assistants: readonly { name: string; home: string }[] | null | undefined): string {
  return (assistants ?? []).map((assistant) => assistantDeleteSentence(assistant.name, assistant.home)).join(' ');
}

/** `description` followed by the assistant sentences, when there are any. */
export function withAssistantNote(description: string, note: string): string {
  return note ? `${description} ${note}` : description;
}

/**
 * The assistant sentences a delete confirmation shows while it is open.
 * `load` starts reading the homes for the dialog being opened; `clear` drops
 * them when it closes, so a late answer for an earlier opening never shows.
 */
export class AssistantDeleteNote {
  #assistants = $state<readonly Thread[]>([]);
  #homes = $state<ReadonlyMap<string, string>>(new Map());
  #generation = 0;

  get note(): string {
    return this.#assistants.map((thread) => assistantDeleteSentence(thread.title, this.#homes.get(thread.id))).join(' ');
  }

  load(threads: readonly Thread[]): void {
    const generation = ++this.#generation;
    this.#assistants = threads.filter((thread) => Boolean(thread.agentId));
    this.#homes = new Map();
    for (const thread of this.#assistants) {
      const backend = threadBackend(thread.id) ?? HOME_BACKEND;
      void withBackendTarget(backend, () => GetStandingAgent(thread.id)).then((agent) => {
        if (generation !== this.#generation || !agent?.home) return;
        const next = new Map(this.#homes);
        next.set(thread.id, agent.home);
        this.#homes = next;
      }).catch((err: unknown) => {
        reportFrontendDiagnostic('assistant home for a delete confirmation failed', userFacingError(err));
      });
    }
  }

  clear(): void {
    this.#generation++;
    this.#assistants = [];
    this.#homes = new Map();
  }
}
