// A Codex worker's async question routed to the thread that spawned it, and
// handed back to the person, under the compact layout. The body is
// standing-agent-worker-async-questions-flow.ts.
import { test } from './fixtures.js';
import { runAsyncQuestionAnsweredBySpawner, runAsyncQuestionHandedBack } from './standing-agent-worker-async-questions-flow.js';

test.beforeEach(async ({ harness }) => {
  // These assertions read agent message rows, which sit inside activity runs.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

test(`an owned Codex worker's async question goes to its lead, which answers it, on a phone`, async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runAsyncQuestionAnsweredBySpawner(harness, page, 'compact', 'claude');
});

test(`an owned Codex worker's async question its lead never took in goes to the person, on a phone`, async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runAsyncQuestionHandedBack(harness, page, 'compact');
});
