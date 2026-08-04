// Which slash commands the provider CLI will execute itself, per thread.
//
// Two sources, deliberately kept apart until the menu unions them:
//
//  1. `provider:commands` — the LIVE per-thread frame. Session state, never
//     history: both wire producers (`system/init`, `system/commands_changed`)
//     restate the whole set, so the newest frame is the whole answer for that
//     thread and nothing is persisted on either side. A thread with no frame
//     means UNKNOWN, which a menu must render differently from "none".
//  2. `GetClaudeSlashCommands()` — the probe-seeded list, available BEFORE any
//     session exists and the only source carrying descriptions and argument
//     hints. It answers for the active Claude identity, not for a thread, so
//     it is cached once app-wide rather than per thread.
//
// Frame storage mirrors fastModeState.svelte.ts exactly, and for the same
// reason: one reactive box per thread (keyedSignalRegistry) rather than a
// SvelteMap, because a MISSING key on a SvelteMap subscribes the reader to the
// whole-map version — so one thread starting a session would invalidate every
// pane's composer. The frames are live SESSION state that outlives a pane
// (a thread switched away from and back keeps its session, and no producer
// re-sends on remount), which is why it lives here and not on ThreadPane.

import { GetClaudeSlashCommands } from './bindings';
import type { ClaudeSlashCommands, SlashCommand } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

/** Payload of the backend `provider:commands` channel. */
export interface ProviderCommandsPayload {
  threadId: string;
  provider?: string;
  /**
   * Always true today, and on the wire because it is the contract: a
   * consumer must never merge one frame into another.
   */
  replace?: boolean;
  commands?: SlashCommand[];
}

/** The last frame a thread received. */
export interface ProviderCommandsFrame {
  provider: string;
  commands: readonly SlashCommand[];
}

const NO_FRAME: ProviderCommandsFrame | undefined = undefined;

const frameByThread = createKeyedSignalRegistry<ProviderCommandsFrame | undefined>(NO_FRAME);

/**
 * Tracked read of a thread's latest frame. `undefined` means no session frame
 * has arrived — unknown, never "this session has no commands". An arrived
 * frame with an empty `commands` array IS a real answer.
 */
export function getProviderCommandsFrame(
  threadId: string | null | undefined,
): ProviderCommandsFrame | undefined {
  if (!threadId) return undefined;
  return frameByThread.get(threadId);
}

/**
 * Apply one `provider:commands` frame. Replace-wholesale: the frame restates
 * the entire set, so the previous one is discarded rather than merged.
 */
export function applyProviderCommands(evt: ProviderCommandsPayload | undefined): void {
  if (!evt?.threadId) return;
  frameByThread.set(evt.threadId, {
    provider: evt.provider ?? '',
    commands: evt.commands ?? [],
  });
}

/** Drop a thread's frame — session teardown, thread delete/archive. */
export function clearProviderCommandsForThread(threadId: string): void {
  if (!threadId) return;
  frameByThread.drop(threadId);
}

// ---------------------------------------------------------------------------
// Probe-seeded Claude list (app-wide, fetched lazily once).

interface ProbeState {
  probed: boolean;
  commands: readonly SlashCommand[];
}

const UNPROBED: ProbeState = { probed: false, commands: [] };

let probeState = $state.raw<ProbeState>(UNPROBED);
let probeInFlight: Promise<void> | null = null;

/**
 * Tracked read of the probe-seeded list. `probed === false` means the probe
 * has not answered for the active Claude identity, so the caller must render
 * nothing provider-specific rather than an empty palette.
 */
export function getClaudeProbeCommands(): ProbeState {
  return probeState;
}

/**
 * Fetch the probe-seeded list once and cache it app-wide. Safe to call on
 * every menu open: the read never spawns a process on the backend and the
 * in-flight promise is shared, so concurrent composers make one RPC.
 *
 * A failure leaves the cache unprobed (still "unknown") and is swallowed:
 * the menu degrades to the thread's own session frame, which is the richer
 * source anyway once a session exists. There is no user action to offer.
 */
export function ensureClaudeProbeCommands(): Promise<void> {
  if (probeState.probed) return Promise.resolve();
  if (probeInFlight) return probeInFlight;
  probeInFlight = GetClaudeSlashCommands()
    .then((answer: ClaudeSlashCommands) => {
      probeState = {
        probed: Boolean(answer?.probed),
        commands: answer?.commands ?? [],
      };
    })
    .catch((err: unknown) => {
      console.warn('GetClaudeSlashCommands failed; command menu falls back to session frames:', err);
    })
    .finally(() => {
      probeInFlight = null;
    });
  return probeInFlight;
}

/** Test-only fixture isolation, matching the sibling live-state stores. */
export function resetForTest(): void {
  frameByThread.reset();
  probeState = UNPROBED;
  probeInFlight = null;
}
