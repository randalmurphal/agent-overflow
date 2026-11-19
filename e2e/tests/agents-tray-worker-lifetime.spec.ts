// A worker that replies and keeps working stays open in its spawner's
// Agents list until it rests (docs/specs/standing-agents.md, "Owned
// workers"). The body is agents-tray-worker-lifetime-helpers.ts.
import { test } from './fixtures.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { runWorkerLifetime } from './agents-tray-worker-lifetime-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker that replies and keeps working stays open until it rests`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    await runWorkerLifetime(harness, page, provider, 'desktop');
  });
}
