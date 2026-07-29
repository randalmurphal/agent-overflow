// runMode tells the SPA whether the desktop binary booted a local
// transport or attached to a remote one (`agent-overflow --connect`).
// Settings panels that mutate local-only state — the LAN-bind toggle
// (NetworkSection), the saved --connect endpoints (RemoteEndpointsSection) —
// must hide / placeholder in client mode because their RPCs would
// otherwise edit the *remote* server's settings instead of the local
// user's.
//
// Source of truth is the bootstrap manifest:
//   - `--connect` boot path injects `window.__AO_BOOTSTRAP__` with
//     `mode: "client"` (see internal/clientmode/clientmode.go).
//   - The local transport's /bootstrap.json omits `mode`; absence is
//     treated as 'local' here.
//
// We read once at module load and cache. The value can't change during
// the SPA's lifetime — a different mode means a different process boot.

import type { RunMode } from './bootstrap';
import { createSubscriber } from 'svelte/reactivity';
export type { RunMode } from './bootstrap';

interface InjectedBootstrap {
  mode?: unknown;
  remote?: unknown;
}

function detectMode(): RunMode {
  if (typeof globalThis === 'undefined') return 'local';
  const injected = (globalThis as { __AO_BOOTSTRAP__?: InjectedBootstrap }).__AO_BOOTSTRAP__;
  const raw = injected?.mode;
  if (raw === 'client' || raw === 'headless' || raw === 'local') return raw;
  return 'local';
}

let cached: RunMode | null = null;
let viewOnly = detectViewOnly();
let notifyViewOnlyChanged: (() => void) | null = null;
const subscribeViewOnly = createSubscriber((update) => {
  notifyViewOnlyChanged = update;
  return () => {
    if (notifyViewOnlyChanged === update) notifyViewOnlyChanged = null;
  };
});

function detectViewOnly(): boolean {
  if (typeof globalThis === 'undefined') return false;
  const injected = (globalThis as { __AO_BOOTSTRAP__?: InjectedBootstrap }).__AO_BOOTSTRAP__;
  return injected?.remote === true;
}

// runMode returns the current run mode. Memoised because the value is
// fixed for the process lifetime; tests that need to switch modes
// between cases call __resetRunModeForTest first.
export function runMode(): RunMode {
  if (cached === null) cached = detectMode();
  return cached;
}

// isClientMode is the convenience the settings panels call. Equivalent
// to `runMode() === 'client'` but reads cleaner at the use-site.
export function isClientMode(): boolean {
  return runMode() === 'client';
}

// isViewOnlySession is reactive when read from a Svelte derived or template.
// Plain remote browsers learn the value asynchronously from /bootstrap.json;
// --connect clients receive the same bit in their injected manifest.
export function isViewOnlySession(): boolean {
  subscribeViewOnly();
  return viewOnly;
}

// Called only by wsClient after it validates a bootstrap manifest. Keeping the
// update at that boundary prevents non-boolean wire values from changing the
// workflow control posture.
export function setViewOnlySessionFromBootstrap(remote: boolean): void {
  if (viewOnly === remote) return;
  viewOnly = remote;
  notifyViewOnlyChanged?.();
}

// __resetRunModeForTest is the test-only escape hatch. Invalidates the
// cached value so a subsequent runMode() call re-reads window state.
// Production code never calls it.
export function __resetRunModeForTest(): void {
  cached = null;
  viewOnly = detectViewOnly();
  notifyViewOnlyChanged?.();
}
