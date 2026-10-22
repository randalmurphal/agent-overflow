// Dictation per computer: whether a computer can transcribe, and the calls
// a composer's DictationSession makes to the computer its thread runs on.
//
// Every call is pinned to that computer. A recording belongs to the
// connection that opened it, so its pieces, finish and cancel must reach
// the same computer as its begin.

import { SvelteMap } from 'svelte/reactivity';
import { BeginDictation, CancelDictation, DictationSources, FinishDictation, type DictationSourceSet } from './bindings';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { uploadDictationPiece } from '../transport/attachmentTransfer';
import { hasScope } from '../transport/scopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { BackendKey } from '../transport/backendKey';
import type { DictationRemote, DictationSession } from '../dictation/dictationSession.svelte';

// What each computer can dictate with once asked, absent until then. A
// sign-in, sign-out or Settings > Voice change there re-reads it.
const sources = new SvelteMap<BackendKey, DictationSourceSet>();
// Each read's generation per computer. Only the newest read applies, so an
// answer from before a sign-in or a detach never lands after it.
const generations = new Map<BackendKey, number>();

/** Whether `backend` can transcribe; false until known. */
export function dictationAvailable(backend: BackendKey): boolean {
  return sources.get(backend)?.subscription ?? false;
}

/** Whether Codex is signed in with ChatGPT on `backend`, whether or not
 * dictation through it is on; false until known. */
export function dictationSignedIn(backend: BackendKey): boolean {
  return sources.get(backend)?.chatGPTSignIn ?? false;
}

/** Ask `backend` again whether it can transcribe; the newest answer wins.
 * A caller without permission to dictate there asks nothing. */
export async function refreshDictationSources(backend: BackendKey): Promise<void> {
  const generation = (generations.get(backend) ?? 0) + 1;
  generations.set(backend, generation);
  if (!hasScope('threads:operate', backend)) {
    sources.delete(backend);
    return;
  }
  try {
    const answer = await withBackendTarget(backend, () => DictationSources());
    if (generations.get(backend) === generation) sources.set(backend, answer);
  } catch (err) {
    if (generations.get(backend) === generation && !isPassiveConnectionFailure(err)) {
      reportFrontendDiagnostic('dictation: could not read whether this computer can transcribe', err instanceof Error ? err.message : '');
    }
  }
}

/** A sign-in, switch, sign-out or Settings > Voice change on `backend` can
 * change what it dictates with; re-read a computer someone has asked about,
 * including one whose first answer has not arrived. */
export function applyDictationSourcesChanged(backend: BackendKey): void {
  if (generations.has(backend)) void refreshDictationSources(backend);
}

onBackendDetached(({ backendId }) => {
  sources.delete(backendId);
  generations.delete(backendId);
});

/** The calls a DictationSession makes, all to `backend`. */
export function dictationRemote(backend: BackendKey): DictationRemote {
  return {
    begin: (container) => withBackendTarget(backend, () => BeginDictation(container)),
    upload: (recordingId, seq, piece) => uploadDictationPiece(backend, recordingId, seq, piece),
    finish: (recordingId, pieces) => withBackendTarget(backend, () => FinishDictation(recordingId, pieces)),
    cancel: (recordingId) => withBackendTarget(backend, () => CancelDictation(recordingId)),
  };
}

// Each pane's composer publishes its session so the dictation chord reaches
// the focused pane's composer, with the same test the composer uses to show
// its mic: the chord never records where the mic is not shown.
interface ComposerDictation {
  session: DictationSession;
  shown: () => boolean;
}

const sessions = new Map<string, ComposerDictation>();

export function registerComposerDictation(paneId: string, session: DictationSession, shown: () => boolean): () => void {
  const entry = { session, shown };
  sessions.set(paneId, entry);
  return () => {
    if (sessions.get(paneId) === entry) sessions.delete(paneId);
  };
}

/** Start or stop dictation in `paneId`'s composer; false when it shows no
 * mic. */
export function toggleComposerDictation(paneId: string | null | undefined): boolean {
  const entry = paneId ? sessions.get(paneId) : undefined;
  if (!entry || !entry.shown()) return false;
  entry.session.toggle();
  return true;
}
