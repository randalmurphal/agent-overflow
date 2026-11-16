// What every surface shares about assistants (docs/specs/standing-agents.md,
// "Standing agent"). A name another assistant holds is refused with a code
// the editors branch on. Deleting an assistant's current thread deletes the
// assistant and removes its home folder ("Deletion, archive and transfer"):
// every confirmation of such a delete names the folder, and the delete is
// always confirmed, whatever the confirmDelete setting says. Any thread's
// delete confirmation counts the workers that go with it ("Owned workers").
// End Session is an assistant thread's own action beside Interrupt
// ("Interrupt and end session").

import { GetDeleteDependents, GetStandingAgent, StopSession } from './bindings';
import { addErrorToast } from './toast.svelte';
import { backendDisplayName, getAttachedBackends } from './attachedBackends.svelte';
import { getThreads } from './threads.svelte';
import { getSettings } from './settings.svelte';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import type { DeleteDependents, Thread } from '../types/models';
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

function counted(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** "a", "a and b", "a, b and c". */
function listed(parts: readonly string[]): string {
  return parts.length < 2 ? parts.join('') : `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
}

/**
 * The sentence for one assistant a delete removes (docs/specs/standing-
 * agents.md ruling 39). Until its record is read the folder is not named
 * and nothing is counted; the dialog shows the full sentence once it is.
 * `dependents` is null when they could not be counted: the sentence still
 * says the threads under it go.
 */
export function assistantDeleteSentence(
  name: string,
  home: string | undefined,
  dependents?: DeleteDependents | null,
): string {
  const folder = home ? `its home folder ${home}` : 'its home folder';
  const under = [
    dependents?.pastThreads ? counted(dependents.pastThreads, 'past thread', 'past threads') : '',
    dependents?.helpers ? counted(dependents.helpers, 'helper', 'helpers') : '',
    dependents?.workers ? counted(dependents.workers, 'worker', 'workers') : '',
  ].filter(Boolean);
  const also = dependents === null
    ? ' with every thread that lives under it,'
    : under.length > 0 ? ` with its ${listed(under)},` : '';
  return `The assistant "${name}" is deleted too,${also} and ${folder} is removed with everything in it.`;
}

/**
 * The sentence for the workers deleted with threads that are not
 * assistants ("Owned workers"), or '' for none. `threads` is how many
 * threads the delete names.
 */
export function workersDeleteSentence(workers: number, threads: number): string {
  if (workers <= 0) return '';
  const owner = threads === 1 ? ['Its', 'it'] : ['Their', 'them'];
  return workers === 1
    ? `${owner[0]} worker is deleted with ${owner[1]}.`
    : `${owner[0]} ${workers} workers are deleted with ${owner[1]}.`;
}

/**
 * What deleting the thread deletes with it, read on the computer it is on.
 * A call that fails as it is made is a rejection, as one that fails later is.
 */
export async function readDeleteDependents(backend: BackendKey, threadId: string): Promise<DeleteDependents> {
  return withBackendTarget(backend, () => GetDeleteDependents(threadId));
}

/** `description` followed by the delete's sentences, when there are any. */
export function withDeleteNote(description: string, note: string): string {
  return note ? `${description} ${note}` : description;
}

/**
 * The sentences a delete confirmation shows while it is open about what
 * goes with the threads: each assistant's home and the threads under it,
 * and the workers of the others. `load` starts the reads for the dialog
 * being opened; `clear` drops them when it closes, so a late answer for an
 * earlier opening never shows. A count that could not be read is null: an
 * assistant's sentence says its threads go, and another thread's workers
 * are left out of the count.
 */
export class ThreadDeleteNote {
  #threads = $state<readonly Thread[]>([]);
  #homes = $state<ReadonlyMap<string, string>>(new Map());
  #dependents = $state<ReadonlyMap<string, DeleteDependents | null>>(new Map());
  #generation = 0;

  get note(): string {
    const sentences = this.#threads
      .filter((thread) => Boolean(thread.agentId))
      .map((thread) => assistantDeleteSentence(thread.title, this.#homes.get(thread.id), this.#dependents.get(thread.id)));
    const workers = this.#threads
      .filter((thread) => !thread.agentId)
      .reduce((sum, thread) => sum + (this.#dependents.get(thread.id)?.workers ?? 0), 0);
    return [...sentences, workersDeleteSentence(workers, this.#threads.length)].filter(Boolean).join(' ');
  }

  load(threads: readonly Thread[]): void {
    const generation = ++this.#generation;
    this.#threads = threads.slice();
    this.#homes = new Map();
    this.#dependents = new Map();
    for (const thread of this.#threads) {
      const backend = threadBackend(thread.id) ?? HOME_BACKEND;
      if (thread.agentId) {
        void withBackendTarget(backend, () => GetStandingAgent(thread.id)).then((agent) => {
          if (generation !== this.#generation || !agent?.home) return;
          const next = new Map(this.#homes);
          next.set(thread.id, agent.home);
          this.#homes = next;
        }).catch((err: unknown) => {
          reportFrontendDiagnostic('assistant home for a delete confirmation failed', userFacingError(err));
        });
      }
      void readDeleteDependents(backend, thread.id).then((counts) => {
        if (generation !== this.#generation || !counts) return;
        this.#setDependents(thread.id, counts);
      }).catch((err: unknown) => {
        reportFrontendDiagnostic('threads deleted with a thread for a delete confirmation failed', userFacingError(err));
        if (generation === this.#generation) this.#setDependents(thread.id, null);
      });
    }
  }

  clear(): void {
    this.#generation++;
    this.#threads = [];
    this.#homes = new Map();
    this.#dependents = new Map();
  }

  #setDependents(threadId: string, counts: DeleteDependents | null): void {
    const next = new Map(this.#dependents);
    next.set(threadId, counts);
    this.#dependents = next;
  }
}
