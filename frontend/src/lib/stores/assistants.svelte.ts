// What every surface shares about assistants (docs/specs/standing-agents.md,
// "Standing agent"). A name another assistant holds is refused with a code
// the editors branch on. Deleting an assistant's current thread deletes the
// assistant and removes its home folder ("Deletion, archive and transfer"):
// every confirmation of such a delete names the folder, and the delete is
// always confirmed, whatever the confirmDelete setting says. End Session is
// an assistant thread's own action beside Interrupt ("Interrupt and end
// session").

import { GetStandingAgent, StopSession } from './bindings';
import { addErrorToast } from './toast.svelte';
import { backendDisplayName, getAttachedBackends } from './attachedBackends.svelte';
import { getThreads } from './threads.svelte';
import { getSettings } from './settings.svelte';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';
import { TransportError } from '../transport/wsClient';

/**
 * End Session: end the thread's provider process with its background agents
 * and shells. A thread with no session is left as it is. Routed by the
 * thread id to the computer that owns it.
 */
export async function endThreadSession(thread: Pick<Thread, 'id' | 'title'>): Promise<void> {
  try {
    await StopSession(thread.id);
  } catch (err) {
    console.error('Failed to end the session:', err);
    addErrorToast(`Could not end the session of ${thread.title || 'this thread'}: ${userFacingError(err)}`, err);
  }
}

/** A name another assistant on the computer holds; the message names it. */
export function isAssistantNameTaken(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'assistant_name_taken';
}

/**
 * A send whose start found the assistant's provider session gone: the
 * assistant moves to a new thread, which holds the message in its composer.
 */
export function isAssistantSessionGone(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'assistant_session_gone';
}

/**
 * A send from an assistant's past thread whose draft the handoff moved: the
 * text waits in the assistant's current thread's composer.
 */
export function isAssistantDraftMoved(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'assistant_draft_moved';
}

/**
 * Two assistant names are the same name, as the store compares them:
 * trimmed, under Unicode simple case folding (strings.EqualFold). Each code
 * point is upper- then lowercased on its own, keeping only one-code-point
 * mappings, so final sigma folds and "ß" does not become "ss".
 * internal/store/testdata/assistant_name_folds.json holds both sides to it.
 */
export function sameAssistantName(a: string, b: string): boolean {
  return foldAssistantName(a) === foldAssistantName(b);
}

function foldAssistantName(name: string): string {
  let out = '';
  for (const char of name.trim()) {
    const upper = char.toUpperCase();
    const single = [...upper].length === 1 ? upper : char;
    const lower = single.toLowerCase();
    out += [...lower].length === 1 ? lower : single;
  }
  return out;
}

/**
 * The name an assistant's row and companion header show: its title, or
 * `<name>-<computer>` while an assistant on another attached computer has
 * the same name. The suffix is display only, never written to a title.
 */
export function assistantDisplayName(thread: Pick<Thread, 'id' | 'title' | 'agentId'>): string {
  if (!thread.agentId) return thread.title;
  const backend = threadBackend(thread.id) ?? HOME_BACKEND;
  const clash = getThreads().some((other) => Boolean(other.agentId) && other.id !== thread.id
    && sameAssistantName(other.title, thread.title) && (threadBackend(other.id) ?? HOME_BACKEND) !== backend);
  if (!clash) return thread.title;
  const entry = getAttachedBackends().find((candidate) => candidate.id === backend);
  return `${thread.title}-${entry ? backendDisplayName(entry) : backend}`;
}

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
