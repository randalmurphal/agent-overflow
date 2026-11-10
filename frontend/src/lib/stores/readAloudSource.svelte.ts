// Read-aloud in OpenAI voices per computer: whether a computer reads
// sentences aloud with its API key, in which voices, and the fetch of one
// sentence's audio from it.
//
// A computer is asked from a mount boundary (the thread surface while
// read-aloud is on here, Settings > Voice for its computer), never from a
// getter. A Settings > Voice change there re-reads it, as does this window
// coming back into view, because the key file changes outside the app.

import { SvelteMap } from 'svelte/reactivity';
import { MintReadAloudTicket, ReadAloudSource, type ReadAloudSourceStatus, type ReadAloudVoice } from './bindings';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { fetchTicketedBytes } from '../transport/attachmentTransfer';
import { hasScope } from '../transport/scopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { BackendKey } from '../transport/backendKey';
import type { FetchClip } from '../native/clipSpeech';

// What each computer answered once asked, absent until then.
const sources = new SvelteMap<BackendKey, ReadAloudSourceStatus>();
// Each read's generation per computer; only the newest read applies.
const generations = new Map<BackendKey, number>();

const NO_VOICES: readonly ReadAloudVoice[] = [];

/** Whether `backend` reads aloud in OpenAI voices now; false until known. */
export function readAloudSourceAvailable(backend: BackendKey): boolean {
  return sources.get(backend)?.available ?? false;
}

/** The OpenAI voices `backend` reads in while available; none otherwise. */
export function readAloudSourceVoices(backend: BackendKey): readonly ReadAloudVoice[] {
  const source = sources.get(backend);
  return source?.available ? source.voices : NO_VOICES;
}

/** Every OpenAI voice `backend` names, whether or not its switch is on;
 * none until known. */
export function readAloudSourceVoiceNames(backend: BackendKey): readonly ReadAloudVoice[] {
  return sources.get(backend)?.voices ?? NO_VOICES;
}

/** Ask `backend` again; the newest answer wins. A caller without
 * permission to operate threads there asks nothing. */
export async function refreshReadAloudSource(backend: BackendKey): Promise<void> {
  const generation = (generations.get(backend) ?? 0) + 1;
  generations.set(backend, generation);
  if (!hasScope('threads:operate', backend)) {
    sources.delete(backend);
    return;
  }
  try {
    const answer = await withBackendTarget(backend, () => ReadAloudSource());
    if (generations.get(backend) === generation) sources.set(backend, answer);
  } catch (err) {
    if (generations.get(backend) === generation && !isPassiveConnectionFailure(err)) {
      reportFrontendDiagnostic('read aloud: could not read whether this computer offers OpenAI voices', err instanceof Error ? err.message : '');
    }
  }
}

/** The switch changed on `backend`: re-read a computer someone has asked
 * about, including one whose first answer has not arrived. */
export function applyReadAloudSourceChanged(backend: BackendKey): void {
  if (generations.has(backend)) void refreshReadAloudSource(backend);
}

/** Re-read every computer asked about when this window comes back into
 * view. Installed once, from App.svelte. */
export function installReadAloudSourceRefresh(): () => void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return () => {};
  const refresh = (): void => {
    if (document.visibilityState !== 'visible') return;
    for (const backend of generations.keys()) void refreshReadAloudSource(backend);
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

/** Fetches one sentence's audio from `backend`: the sentence is admitted
 * there, then its ticketed URL is spent once. A failure's message is the
 * computer's reason. */
export function readAloudClipFetcher(backend: BackendKey): FetchClip {
  return async (text, voice, signal) => {
    const url = await withBackendTarget(backend, () => MintReadAloudTicket(text, voice));
    signal.throwIfAborted();
    return fetchTicketedBytes(backend, url, null, signal);
  };
}

/** Forget every computer. Tests only. */
export function resetReadAloudSourcesForTest(): void {
  sources.clear();
  generations.clear();
}
