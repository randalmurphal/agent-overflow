// A worker whose turn ended while its background shell runs, or with a loop
// armed, or whose own worker has a loop armed, stays open in its spawner's Agents list as waiting, under the
// compact layout (docs/specs/standing-agents.md, "Owned workers"), and its
// spawn is not answered until it rests (docs/specs/agent-thread-tools.md),
// or until a later turn of it fails; a worker's row counts its tool calls,
// tokens and runtime as they move and shows its latest tool call. The body
// is agents-tray-worker-waiting-helpers.ts. The layout is the same on both
// providers, so this runs on the Claude mock; each provider's own frames
// are agents-tray-worker-waiting.spec.ts's, on both.
// Its backend's settled rows linger AGENT_ROW_LINGER_MS (agents-tray-helpers.ts).
import { test } from './agents-tray-helpers.js';
import { runAncestorWaiting, runHeldThenError, runHeldWorkerFacts, runReplyThenRest, runWorkerWaiting } from './agents-tray-worker-waiting-helpers.js';

test('claude: a worker waiting on its background shell or its loop stays open, on a phone', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runWorkerWaiting(harness, page, 'claude', 'compact');
});

test('claude: a worker whose own worker waits on its loop stays open, waiting on it, on a phone', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runAncestorWaiting(harness, page, 'claude', 'compact');
});

test("claude: a worker's row counts what it did, and its spawn finishes only once its shell ends, on a phone", async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runHeldWorkerFacts(harness, page, 'claude', 'compact');
});

test("claude: a worker that replies and works on stays open, then rests with its turn's runtime, on a phone", async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runReplyThenRest(harness, page, 'claude', 'compact');
});

test("claude: a spawn held for its worker's loop settles failed when a later turn fails, on a phone", async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runHeldThenError(harness, page, 'claude', 'compact');
});
