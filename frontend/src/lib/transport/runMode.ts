// runMode tells the SPA whether the desktop binary booted a local
// transport or attached to a remote one (`agent-overflow --connect`).
// Settings panels that mutate local-only state — the LAN-bind toggle
// (NetworkSection) is the standing example — must hide / placeholder in
// client mode because their RPCs would
// otherwise edit the *remote* server's settings instead of the local
// user's.
//
// Source of truth is the `?mode=` parameter on the page URL:
//   - The `--connect` stub stamps `mode=client` on the URL it hands its
//     webview (internal/clientmode/clientmode.go AppURL).
//   - Every other boot path omits it; absence is 'local'.
//
// It rides the URL rather than the bootstrap manifest because this is
// read once at module load, synchronously, before any fetch resolves —
// the same reason `?cid=` carries the client identity. It survives the
// bootstrap ticket's scrub: only the ticket parameter is removed.
//
// We read once at module load and cache. The value can't change during
// the SPA's lifetime — a different mode means a different process boot.
//
// This is a PROCESS-BOOT fact and nothing else. What a session is allowed
// to do is a different axis entirely and lives in ./scopes.ts: run mode
// answers "whose settings would this RPC edit", capabilities answer "was
// this session granted this". Conflating them is how a `--connect` client
// attached to a LOCAL backend — which may do everything — ends up gated
// like a remote viewer.

// RunMode marks how the SPA is attached to its backend:
//   - 'local'    — the desktop binary booted a local transport in the
//                  same process, or a browser is attached straight to
//                  one. The default whenever the URL omits the mode.
//   - 'client'   — desktop binary launched with --connect; the local
//                  process owns only a stub HTTP server and the SPA's
//                  RPCs are carried to a remote backend. Local-only
//                  settings panels must hide / placeholder in this mode.
//   - 'headless' — reserved for the WSL launcher path. Not currently
//                  stamped by any boot flow, but defined here so a
//                  future flow can mark itself without an enum widening.
export type RunMode = 'local' | 'client' | 'headless';

function detectMode(): RunMode {
  if (typeof window === 'undefined' || typeof window.location === 'undefined') return 'local';
  const raw = new URLSearchParams(window.location.search).get('mode');
  if (raw === 'client' || raw === 'headless' || raw === 'local') return raw;
  return 'local';
}

let cached: RunMode | null = null;

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

// __resetRunModeForTest is the test-only escape hatch. Invalidates the
// cached value so a subsequent runMode() call re-reads the page URL.
// Production code never calls it.
export function __resetRunModeForTest(): void {
  cached = null;
}
