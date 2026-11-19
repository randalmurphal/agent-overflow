// A worker whose turn ended while its background shell runs, or with a loop
// armed, or whose own worker has a loop armed, stays open in its spawner's Agents list as waiting
// (docs/specs/standing-agents.md, "Owned workers"), and its spawn is not
// answered until it rests (docs/specs/agent-thread-tools.md), or until a
// later turn of it fails; a worker's row counts its tool calls, tokens and
// runtime as they move and shows its latest tool call. The body is
// agents-tray-worker-waiting-helpers.ts.
import { test } from './fixtures.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { runAncestorWaiting, runHeldThenError, runHeldWorkerFacts, runReplyThenRest, runWorkerWaiting } from './agents-tray-worker-waiting-helpers.js';

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

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker that replies and works on stays open, then rests with its turn's runtime`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runReplyThenRest(harness, page, provider, 'desktop');
  });
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a spawn held for its worker's loop settles failed when a later turn fails`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runHeldThenError(harness, page, provider, 'desktop');
  });
}
