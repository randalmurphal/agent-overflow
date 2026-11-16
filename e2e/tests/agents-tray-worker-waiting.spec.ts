// A worker whose turn ended while its background shell runs, or with a loop
// armed, or whose own worker has a loop armed, stays open in its spawner's Agents list as waiting
// (docs/specs/standing-agents.md, "Owned workers"), and its spawn is not
// answered until it rests (docs/specs/agent-thread-tools.md); a worker's
// row counts its tool calls and tokens and shows its latest tool call. The
// body is agents-tray-worker-waiting-helpers.ts.
import { test } from './fixtures.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { runAncestorWaiting, runHeldWorkerFacts, runWorkerWaiting } from './agents-tray-worker-waiting-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker waiting on its background shell or its loop stays open`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runWorkerWaiting(harness, page, provider, 'desktop');
  });
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker whose own worker waits on its loop stays open, waiting on it`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runAncestorWaiting(harness, page, provider, 'desktop');
  });
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker's row counts what it did, and its spawn finishes only once its shell ends`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runHeldWorkerFacts(harness, page, provider, 'desktop');
  });
}
