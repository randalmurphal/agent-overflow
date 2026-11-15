// A worker whose turn ended while its background shell runs, or with a loop
// armed, or whose own worker has a loop armed, stays open in its spawner's Agents list as waiting, under the
// compact layout (docs/specs/standing-agents.md, "Owned workers"). The body
// is agents-tray-worker-waiting-helpers.ts.
import { test } from './fixtures.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { runAncestorWaiting, runWorkerWaiting } from './agents-tray-worker-waiting-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker waiting on its background shell or its loop stays open, on a phone`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runWorkerWaiting(harness, page, provider, 'compact');
  });
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker whose own worker waits on its loop stays open, waiting on it, on a phone`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runAncestorWaiting(harness, page, provider, 'compact');
  });
}
