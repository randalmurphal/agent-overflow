// Dictation per computer: whether a computer can transcribe, and the calls
// a composer's DictationSession makes to the computer its thread runs on.
//
// Every call is pinned to that computer. A recording belongs to the
// connection that opened it, so its pieces, finish and cancel must reach
// the same computer as its begin.

import { SvelteMap } from 'svelte/reactivity';
import {
  AppendLiveDictation,
  BeginDictation,
  BeginLiveDictation,
  CancelDictation,
  CancelLiveDictation,
  DictationSources,
  FinishDictation,
  FinishLiveDictation,
  type DictationSourceSet,
} from './bindings';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { uploadDictationPiece } from '../transport/attachmentTransfer';
import { hasScope } from '../transport/scopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { TransportError } from '../transport/wsClient';
import { bytesToBase64 } from '../utils/base64';
import type { BackendKey } from '../transport/backendKey';
import type { DictationRemote, DictationSession, DictationSourceKind } from '../dictation/dictationSession.svelte';

// What each computer can dictate with once asked, absent until then. A
// sign-in, sign-out or Settings > Voice change there re-reads it.
const sources = new SvelteMap<BackendKey, DictationSourceSet>();
// Each read's generation per computer. Only the newest read applies, so an
// answer from before a sign-in or a detach never lands after it.
const generations = new Map<BackendKey, number>();

/** The computer's code for a live recording that switching live off ended
 * (internal/dictation, LiveSwitchedOffCode). */
export const LIVE_SWITCHED_OFF = 'live_dictation_switched_off';

/** What this engine can capture: a recording for the computer to
 * transcribe whole, and PCM to stream live. */
export interface DictationEngine {
  records: boolean;
  streamsPcm: boolean;
}

/** Whether `backend` dictates through its Codex sign-in; false until
 * known. */
export function dictationSubscriptionReady(backend: BackendKey): boolean {
  return sources.get(backend)?.subscription ?? false;
}

/** Whether `backend` dictates live through its OpenAI API key; false until
 * known. */
export function dictationLiveReady(backend: BackendKey): boolean {
  return sources.get(backend)?.live ?? false;
}

/** How a recording on this engine is transcribed by `backend`, or null
 * when it cannot be. Live wins when both are on: turning it on is asking
 * for it. An engine that cannot stream records instead where it can. */
export function dictationSource(backend: BackendKey, engine: DictationEngine): DictationSourceKind | null {
  const known = sources.get(backend);
  if (known?.live && engine.streamsPcm) return 'live';
  if (known?.subscription && engine.records) return 'recorded';
  return null;
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

/** The live source also needs the key file, which the user edits outside
 * the app, so every computer asked about is re-read when this window comes
 * back into view. Installed once, from App.svelte. */
export function installDictationSourcesRefresh(): () => void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return () => {};
  const refresh = (): void => {
    if (document.visibilityState !== 'visible') return;
    for (const backend of generations.keys()) void refreshDictationSources(backend);
  };
  window.addEventListener('focus', refresh);
  document.addEventListener('visibilitychange', refresh);
  return () => {
    window.removeEventListener('focus', refresh);
    document.removeEventListener('visibilitychange', refresh);
  };
}

onBackendDetached(({ backendId }) => {
  sources.delete(backendId);
  generations.delete(backendId);
});

/** The calls a DictationSession makes, all to `backend`. */
export function dictationRemote(backend: BackendKey, engine: DictationEngine): DictationRemote {
  return {
    source: () => dictationSource(backend, engine) ?? 'recorded',
    begin: (container) => withBackendTarget(backend, () => BeginDictation(container)),
    upload: (recordingId, seq, piece) => uploadDictationPiece(backend, recordingId, seq, piece),
    finish: (recordingId, pieces) => withBackendTarget(backend, () => FinishDictation(recordingId, pieces)),
    cancel: (recordingId) => withBackendTarget(backend, () => CancelDictation(recordingId)),
    live: {
      begin: () => withBackendTarget(backend, () => BeginLiveDictation()),
      // The transport carries bytes as base64, as Go encodes []byte.
      append: (recordingId, seq, pcm) =>
        withBackendTarget(backend, () => AppendLiveDictation(recordingId, seq, bytesToBase64(pcm))),
      finish: (recordingId) => withBackendTarget(backend, () => FinishLiveDictation(recordingId)),
      cancel: (recordingId) => withBackendTarget(backend, () => CancelLiveDictation(recordingId)),
      stillOn: async () => {
        await refreshDictationSources(backend);
        return dictationLiveReady(backend);
      },
      endedBySwitch: (err) => err instanceof TransportError && err.code === LIVE_SWITCHED_OFF,
    },
  };
}

// Each pane's composer publishes its session so the dictation chord reaches
// the focused pane's composer, with the same test the composer uses to show
// its mic: the chord never records where the mic is not shown.
interface ComposerDictation {
  backend: BackendKey;
  session: DictationSession;
  shown: () => boolean;
}

const sessions = new Map<string, ComposerDictation>();

export function registerComposerDictation(
  paneId: string,
  backend: BackendKey,
  session: DictationSession,
  shown: () => boolean,
): () => void {
  const entry = { backend, session, shown };
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
