// Latest per-thread fast-mode report from the provider.
//
// Live session state, never history: the backend restates it on every
// `system/init` and every turn-complete, so the newest frame is the whole
// answer and nothing is persisted on either side. A thread with no entry
// means "unknown", which callers must render differently from "off" —
// see utils/fastMode.ts.
//
// One reactive box per thread (keyedSignalRegistry) rather than a
// SvelteMap: the composer toolbar reads this per pane, and a MISSING key
// on a SvelteMap subscribes the reader to the whole-map version, so any
// thread's turn ending would invalidate every pane's toolbar.

import type { FastModeReport } from '../utils/fastMode';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

export interface FastModeStatePayload {
  threadId: string;
  state?: string;
  disabledReason?: string;
}

const NO_REPORT: FastModeReport | undefined = undefined;

const reportByThread = createKeyedSignalRegistry<FastModeReport | undefined>(NO_REPORT);

/** Tracked read of a thread's latest report; undefined when unknown. */
export function getFastModeReport(
  threadId: string | null | undefined,
): FastModeReport | undefined {
  if (!threadId) return undefined;
  return reportByThread.get(threadId);
}

/**
 * Apply a `provider:fast_mode` frame. The backend only emits when the
 * wire actually carried a report, so a frame is always a real signal —
 * but guard the empty case anyway rather than storing a report that says
 * nothing, which would flip a thread from "unknown" to a falsy-state
 * report that reads as a denial.
 */
export function applyFastModeState(evt: FastModeStatePayload | undefined): void {
  if (!evt || !evt.threadId) return;
  const state = (evt.state ?? '').trim();
  const disabledReason = (evt.disabledReason ?? '').trim();
  if (state === '' && disabledReason === '') return;
  reportByThread.set(evt.threadId, { state, disabledReason });
}

/** Drop a thread's report — session teardown, thread delete/archive. */
export function clearFastModeStateForThread(threadId: string): void {
  if (!threadId) return;
  reportByThread.drop(threadId);
}

/** Test-only fixture isolation, matching the sibling stores. */
export function resetForTest(): void {
  reportByThread.reset();
}
