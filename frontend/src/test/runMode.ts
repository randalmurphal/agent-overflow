// Test helper for switching the SPA's runMode between local / client.
// runMode reads `window.__AO_BOOTSTRAP__.mode` once and memoises, so any
// test that toggles between modes needs both a globalThis tweak AND a
// cache invalidation. Centralising the pair here keeps the three
// settings-section tests from drifting on copy-paste.

import type { RunMode } from '../lib/transport/bootstrap';
import { __resetRunModeForTest } from '../lib/transport/runMode';

// setRunMode flips the bootstrap-injected mode the SPA reads. 'client'
// installs a stub bootstrap, anything else (or 'local') strips it.
// __resetRunModeForTest invalidates the runMode cache so the next read
// picks up the change.
export function setRunMode(mode: RunMode): void {
  if (mode === 'client') {
    (globalThis as { __AO_BOOTSTRAP__?: { mode?: string } }).__AO_BOOTSTRAP__ = { mode: 'client' };
  } else {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
  }
  __resetRunModeForTest();
}

// resetRunMode is the convenience reset. Equivalent to setRunMode('local')
// but reads cleaner at the call site of beforeEach / afterEach hooks
// where the intent is "back to default".
export function resetRunMode(): void {
  setRunMode('local');
}
