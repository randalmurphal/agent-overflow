// An assistant's Previous Threads dialog (docs/specs/standing-agents.md,
// "Sidebar"), from its row's menu: the threads earlier handoffs superseded,
// newest first, read a page at a time from the assistant's computer
// (ListAssistantPastThreads). Picking one opens it in the pane the menu came
// from and closes the dialog.
//
// Each opening is its own object, as New Assistant's are: a page that
// answers after the dialog closed, or after it was opened again, is
// dropped, and only the opening that asked may show its rows or its error.
// The dialog follows the assistant, not one of its threads, so a handoff
// while it is open keeps it.

import { ListAssistantPastThreads, type PastThreadsCursor } from './bindings';
import { openThreadFromNavigation } from './panes.svelte';
import type { ThreadPane } from './thread.svelte';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { noteThread, threadBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { userFacingError } from '../utils/userFacingError';
import { addErrorToast } from './toast.svelte';
import { assistantsShown } from './assistants.svelte';

/** Rows one page reads, and one "Show N more…" reveals. */
export const PREVIOUS_THREADS_PAGE = 20;

/** One opening of the dialog and what it has read. Replaced whole. */
export interface PreviousThreadsOpening {
  readonly agentId: string;
  /** The computer that holds the assistant, captured when the dialog opened. */
  readonly backend: BackendKey;
  /** The pane the menu came from; a picked thread opens there. */
  readonly targetPane: ThreadPane | null;
}

export interface PreviousThreadsState {
  readonly opening: PreviousThreadsOpening;
  /** Newest superseded first. */
  readonly threads: readonly Thread[];
  /** Where the next page starts; null once the last page has landed. */
  readonly next: PastThreadsCursor | null;
  /** A page is being read. */
  readonly loading: boolean;
  /** The first page has landed. */
  readonly loaded: boolean;
  readonly error: string | null;
}

// Raw: an opening's identity is what a late page compares.
let state: PreviousThreadsState | null = $state.raw(null);

export function getPreviousThreads(): PreviousThreadsState | null {
  return state;
}

/**
 * Opens the dialog for the assistant whose current thread this is and reads
 * its first page; any other thread has none. An unknown owner is reported
 * as the dialog's error rather than read from another computer. Nothing
 * opens while assistants are turned off.
 */
export function openPreviousThreadsDialog(thread: Pick<Thread, 'id' | 'agentId'>, targetPane: ThreadPane | null): void {
  if (!thread.agentId || !assistantsShown()) return;
  let backend: BackendKey;
  try {
    backend = requireEntityBackend(threadBackend(thread.id));
  } catch (err) {
    addErrorToast(userFacingError(err), err);
    return;
  }
  const opening: PreviousThreadsOpening = { agentId: thread.agentId, backend, targetPane };
  state = { opening, threads: [], next: { supersededAt: 0, threadId: '' }, loading: false, loaded: false, error: null };
  void readPage(opening);
}

/** Reads the next page of the open dialog; nothing while one is out or none is left. */
export function showMorePreviousThreads(): void {
  if (state && !state.loading && state.next !== null) void readPage(state.opening);
}

/** Closes the dialog; given an opening, only while it is still the current one. */
export function closePreviousThreadsDialog(opening?: PreviousThreadsOpening): void {
  if (opening === undefined || state?.opening === opening) state = null;
}

/** Opens a listed thread where the menu was opened, and closes the dialog. */
export async function openPreviousThread(opening: PreviousThreadsOpening, thread: Thread): Promise<void> {
  closePreviousThreadsDialog(opening);
  try {
    await openThreadFromNavigation(thread, opening.targetPane);
  } catch (err) {
    console.error('Failed to open a previous thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}

async function readPage(opening: PreviousThreadsOpening): Promise<void> {
  const current = state;
  if (current?.opening !== opening || current.next === null) return;
  const after = current.next;
  state = { ...current, loading: true, error: null };
  try {
    const page = await withBackendTarget(opening.backend, () => ListAssistantPastThreads(opening.agentId, after, PREVIOUS_THREADS_PAGE));
    if (state?.opening !== opening) return;
    const rows = (page?.threads ?? []) as Thread[];
    // The pane a row opens in routes its calls by the thread's computer.
    for (const row of rows) noteThread(row.id, opening.backend, row.ownershipEpoch ?? 0);
    state = { ...state, threads: [...state.threads, ...rows], next: page?.next ?? null, loading: false, loaded: true };
  } catch (err) {
    if (state?.opening !== opening) return;
    console.error('Failed to load previous threads:', err);
    state = { ...state, loading: false, error: userFacingError(err) };
  }
}

/** Test helper. */
export function resetPreviousThreadsForTest(): void {
  state = null;
}
