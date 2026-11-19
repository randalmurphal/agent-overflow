// Whether each computer's providers keep their own memory, as that computer
// reads its Claude and Codex configuration (GetNativeMemoryStatus). An
// assistant's banner and Settings > Memory watch a computer while they show.
// A watched computer is read when a surface starts watching it, again when
// its settings change (a provider environment edit changes the answer), when
// it reconnects or its settings frames had a gap, and when this window comes
// back into view, because the provider configuration changes outside the
// app. A dropped connection keeps the last answer; the reconnect reads it
// again. The answer is dropped with its last watcher.

import { SvelteMap } from 'svelte/reactivity';
import { GetNativeMemoryStatus } from './bindings';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { errString } from '../utils/errors';
import type { BackendKey } from '../transport/backendKey';
import type { NativeMemoryStatus } from '../types/models';

/** A computer's answer: each provider's own memory, null from a release
 * that cannot say, or why the computer could not be read at all. */
export interface ProviderMemoryAnswer {
  status: NativeMemoryStatus | null;
  error: string;
}

const answers = new SvelteMap<BackendKey, ProviderMemoryAnswer>();
const watchers = new Map<BackendKey, number>();
// Each read's generation per computer; only the newest read applies, and
// none after the last watcher leaves.
const generations = new Map<BackendKey, number>();

/** `backend`'s latest answer while watched; undefined until it lands. */
export function providerMemory(backend: BackendKey): ProviderMemoryAnswer | undefined {
  return answers.get(backend);
}

function nextGeneration(backend: BackendKey): number {
  const generation = (generations.get(backend) ?? 0) + 1;
  generations.set(backend, generation);
  return generation;
}

async function read(backend: BackendKey): Promise<void> {
  const generation = nextGeneration(backend);
  try {
    const status = await withBackendTarget(backend, () => GetNativeMemoryStatus());
    if (generations.get(backend) === generation) answers.set(backend, { status, error: '' });
  } catch (err) {
    if (generations.get(backend) !== generation || isPassiveConnectionFailure(err)) return;
    answers.set(backend, { status: null, error: errString(err) });
    reportFrontendDiagnostic('Could not read whether a provider\'s own memory is on', errString(err));
  }
}

function refreshVisible(): void {
  if (document.visibilityState !== 'visible') return;
  for (const backend of watchers.keys()) void read(backend);
}

/** Watch `backend` until the returned release: reads it now and keeps the
 * answer current. A caller without `settings:read` there must not watch. */
export function watchProviderMemory(backend: BackendKey): () => void {
  if (watchers.size === 0) {
    window.addEventListener('focus', refreshVisible);
    document.addEventListener('visibilitychange', refreshVisible);
  }
  watchers.set(backend, (watchers.get(backend) ?? 0) + 1);
  void read(backend);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const left = (watchers.get(backend) ?? 1) - 1;
    if (left > 0) {
      watchers.set(backend, left);
      return;
    }
    watchers.delete(backend);
    nextGeneration(backend);
    answers.delete(backend);
    if (watchers.size === 0) {
      window.removeEventListener('focus', refreshVisible);
      document.removeEventListener('visibilitychange', refreshVisible);
    }
  };
}

/** `backend`'s settings changed, it reconnected, or its settings frames had
 * a gap: read it again while watched. */
export function refreshProviderMemory(backend: BackendKey): void {
  if (watchers.has(backend)) void read(backend);
}

onBackendDetached(({ backendId }) => {
  nextGeneration(backendId);
  answers.delete(backendId);
});
