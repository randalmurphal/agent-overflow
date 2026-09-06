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
//     it is cached once per computer rather than per thread.
//
// Frame storage mirrors fastModeState.svelte.ts exactly, and for the same
// reason: one reactive box per thread (keyedSignalRegistry) rather than a
// SvelteMap, because a MISSING key on a SvelteMap subscribes the reader to the
// whole-map version — so one thread starting a session would invalidate every
// pane's composer. The frames are live SESSION state that outlives a pane
// (a thread switched away from and back keeps its session, and no producer
// re-sends on remount), which is why it lives here and not on ThreadPane.

import type { BackendKey } from '../transport/backendKey';
import { backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
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
// Probe-seeded Claude list (per computer, fetched lazily).

interface ProbeState {
  probed: boolean;
  commands: readonly SlashCommand[];
}

const UNPROBED: ProbeState = { probed: false, commands: [] };

const probeStates = createKeyedSignalRegistry<ProbeState>(UNPROBED);
const probeInFlight = new Map<BackendKey, Promise<void>>();

export function invalidateClaudeProbeCommands(backend: BackendKey): void {
  probeInFlight.delete(backend);
  probeStates.drop(backend);
}
onBackendDetached(({ backendId }) => invalidateClaudeProbeCommands(backendId));

/**
 * Tracked read of the probe-seeded list. `probed === false` means the probe
 * has not answered for the active Claude identity, so the caller must render
 * nothing provider-specific rather than an empty palette.
 */
export function getClaudeProbeCommands(backend: BackendKey): ProbeState {
  return probeStates.get(backend);
}

/**
 * Fetch the probe-seeded list once and cache it for that computer. Safe to call on
 * every menu open: the read never spawns a process on the backend and the
 * in-flight promise is shared, so concurrent composers make one RPC.
 *
 * A failure leaves the cache unprobed (still "unknown") and is swallowed:
 * the menu degrades to the thread's own session frame, which is the richer
 * source anyway once a session exists. There is no user action to offer.
 */
export function ensureClaudeProbeCommands(backend: BackendKey): Promise<void> {
  if (!hasScope('threads:read', backend) || probeStates.get(backend).probed) return Promise.resolve();
  const held = probeInFlight.get(backend);
  if (held) return held;
  const target = backendById(backend);
  const request = withBackendTarget(backend, () => GetClaudeSlashCommands())
    .then((answer: ClaudeSlashCommands) => {
      if (backendById(backend) !== target || probeInFlight.get(backend) !== request) return;
      probeStates.set(backend, {
        probed: Boolean(answer?.probed),
        commands: answer?.commands ?? [],
      });
    })
    .catch((err: unknown) => {
      if (probeInFlight.get(backend) !== request) return;
      console.warn('GetClaudeSlashCommands failed; command menu falls back to session frames:', err);
    })
    .finally(() => {
      if (probeInFlight.get(backend) === request) probeInFlight.delete(backend);
    });
  probeInFlight.set(backend, request);
  return request;
}

/** Test-only fixture isolation, matching the sibling live-state stores. */
export function resetForTest(): void {
  frameByThread.reset();
  probeStates.reset();
  probeInFlight.clear();
}
