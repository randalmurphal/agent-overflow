// Process boot mode is fixed by the page URL and independent of authorization.
// Paired --connect windows use a local frontend controller with independent
// computer connections. Legacy launch-token URLs use the single-upstream relay.
// The query survives ticket scrubbing; absence means an ordinary app/browser.
// Capabilities and grants belong to scopes.ts, never inferred from this mode.
//
// local: ordinary desktop or direct browser; client: legacy token relay;
// frontend: local connection/presentation controller with no execution host;
// headless: reserved for a future launcher that explicitly stamps it.
export type RunMode = 'local' | 'client' | 'headless' | 'frontend';

function detectMode(): RunMode {
  if (typeof window === 'undefined' || typeof window.location === 'undefined') return 'local';
  const raw = new URLSearchParams(window.location.search).get('mode');
  if (raw === 'client' || raw === 'headless' || raw === 'local' || raw === 'frontend') return raw;
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

// Both client entry points lack an execution host in the window's process.
export function isClientMode(): boolean {
  return runMode() === 'client' || isFrontendOnly();
}

/** A local desktop controller with no execution backend of its own. */
export function isFrontendOnly(): boolean { return runMode() === 'frontend'; }

/** The explicit --connect target is a computer, never the local controller. */
export function initialComputer(): string {
  if (!isFrontendOnly()) return '';
  return new URLSearchParams(window.location.search).get('computer') ?? '';
}

// __resetRunModeForTest is the test-only escape hatch. Invalidates the
// cached value so a subsequent runMode() call re-reads the page URL.
// Production code never calls it.
export function __resetRunModeForTest(): void {
  cached = null;
}
