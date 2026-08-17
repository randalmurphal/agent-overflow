// Thread-title generation pending state, keyed by THREAD (entity-keyed —
// the same generation is one fact however many surfaces render it, and the
// regenerate affordance must keep spinning across a pane switch or remount).
//
// The backend's `RegenerateThreadTitle` is an async ack: the RPC validates
// and claims the thread's in-flight slot, then returns while the provider
// CLI runs for up to two 3-minute attempts — far past the transport's flat
// RPC timeout, which is why the completion arrives as the
// `thread:title_generation` event instead of the RPC's answer. The event
// fires for EVERY generation run (auto first-turn, heal, regeneration), so a
// click that "joins" an already-running generation is cleared by that run's
// completion frame. A keyedSignalRegistry fits because the state is
// push-fed with nothing to acquire or release.
//
// Failures surface only when the user asked: the pending flag doubles as
// "a user is awaiting this run", so auto-generation failures (already
// logged backend-side) stay quiet while a clicked regeneration reports on
// the pane showing the thread — or a toast when that pane is gone by the
// time the run settles.

import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { RegenerateThreadTitle } from './bindings';
import { findPaneShowingThread } from './panes.svelte';
import { onTransportStatusChange } from './transportStatus.svelte';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';

/** Completion frame of one backend title-generation run. */
export interface ThreadTitleGenerationEvent {
  threadId: string;
  /** Redacted failure text; empty on success and on the no-op outcomes. */
  error: string;
}

const pending = createKeyedSignalRegistry<boolean>(false);

// Which keys currently hold a true flag. The registry deliberately has no
// key enumeration (and its reset() is test-only), and the reconnect sweep
// below needs to release exactly the flags that are up.
const awaited = new Set<string>();

function setPending(threadId: string, value: boolean): void {
  if (value) awaited.add(threadId);
  else awaited.delete(threadId);
  pending.set(threadId, value);
}

/**
 * Release every raised pending flag without surfacing anything. For the two
 * transport edges where a completion frame may have been LOST — the
 * reconnect below, and a mid-connection event-buffer gap on the channel
 * (eventsTransportGap.ts). The completion event is the only ordinary
 * release, so a lost frame otherwise leaves the affordance a disabled
 * spinner forever. Pessimistic release is safe both ways: a re-click on a
 * run that is in fact still live just joins its backend claim (the ack
 * returns without starting a second run).
 */
export function releaseThreadTitleGenerationPending(): void {
  for (const threadId of [...awaited]) setPending(threadId, false);
}

let wasDown = false;
onTransportStatusChange((snapshot) => {
  if (snapshot.status !== 'connected') {
    wasDown = true;
    return;
  }
  if (!wasDown) return;
  wasDown = false;
  releaseThreadTitleGenerationPending();
});

/** Tracked read: is a title generation the user asked for in flight? */
export function titleGenerationPending(threadId: string): boolean {
  return pending.get(threadId);
}

/**
 * Ask the backend to re-title the thread from its conversation so far.
 *
 * Pending is set before the ack so the affordance can't double-fire in the
 * dispatch gap; the completion event is what clears it. Only a rejected ack
 * (unknown thread, transport refusal) clears it here, since no run started.
 */
export async function regenerateThreadTitle(threadId: string): Promise<void> {
  if (pending.get(threadId)) return;
  setPending(threadId, true);
  try {
    await RegenerateThreadTitle(threadId);
  } catch (err) {
    console.error('Regenerate thread title failed:', err);
    setPending(threadId, false);
    surfaceFailure(threadId, errString(err));
  }
}

/** `thread:title_generation` handler (wired in events.ts). */
export function applyThreadTitleGeneration(event: ThreadTitleGenerationEvent | null): void {
  const threadId = event?.threadId;
  if (!threadId) return;
  const wasAwaited = pending.get(threadId);
  setPending(threadId, false);
  if (event.error && wasAwaited) surfaceFailure(threadId, event.error);
}

function surfaceFailure(threadId: string, message: string): void {
  const text = `Failed to regenerate title: ${message}`;
  const pane = findPaneShowingThread(threadId);
  if (pane) pane.setGeneralError(text);
  else addToast('error', text);
}

export function resetThreadTitleGenerationForTest(): void {
  awaited.clear();
  wasDown = false;
  pending.reset();
}
