// Agent profiles and the sub-thread limit, end to end on the desktop
// layout (docs/specs/agent-thread-tools.md, "Agent profiles" and
// "Sub-thread depth"): a profile edited in Settings reaches thread_options,
// each roster list keeps its draft when the roster changes elsewhere, a
// spawn by a profile lands on its provider and model with its instructions,
// the spawn row shows them from the spawn's reply after the worker is
// deleted, a worker or ask copy at the limit has no thread_spawn, and
// Restore puts back the computer's own starting roster. The body is
// agent-profiles-helpers.ts.
import { test } from './fixtures.js';
import { runAgentProfiles } from './agent-profiles-helpers.js';

test('a profile edited in Settings is what a spawn by it runs, and a worker at the limit cannot spawn', async ({ harness, page }) => {
  test.setTimeout(180_000);
  await runAgentProfiles(harness, page, 'desktop');
});
