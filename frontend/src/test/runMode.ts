// Test helper for switching the SPA's runMode between local / client.
// runMode reads `?mode=` off the page URL once and memoises, so any test
// that toggles between modes needs both a URL rewrite AND a cache
// invalidation. Centralising the pair here keeps the settings-section
// tests from drifting on copy-paste.

import type { RunMode } from '../lib/transport/runMode';
import { __resetRunModeForTest } from '../lib/transport/runMode';

// setRunMode stamps the mode the SPA reads onto the document URL.
// 'client' adds `?mode=client`, anything else clears it.
// __resetRunModeForTest invalidates the runMode cache so the next read
// picks up the change.
export function setRunMode(mode: RunMode): void {
  if (typeof window !== 'undefined' && typeof window.history?.replaceState === 'function') {
    const search = mode === 'local' ? '' : `?mode=${mode}`;
    window.history.replaceState(null, '', window.location.pathname + search);
  }
  __resetRunModeForTest();
}

// resetRunMode is the convenience reset. Equivalent to setRunMode('local')
// but reads cleaner at the call site of beforeEach / afterEach hooks
// where the intent is "back to default".
export function resetRunMode(): void {
  setRunMode('local');
}
