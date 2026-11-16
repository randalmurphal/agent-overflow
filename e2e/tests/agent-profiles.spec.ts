// Agent profiles and the sub-thread limit, end to end on the desktop
// layout (docs/specs/agent-thread-tools.md, "Agent profiles" and
// "Sub-thread depth"): a profile edited in Settings reaches thread_options,
// a spawn by it lands on its provider and model with its instructions, the
// spawn row shows them, and a worker at the limit has no thread_spawn. The
// body is agent-profiles-helpers.ts.
import { test } from './fixtures.js';
import { runAgentProfiles } from './agent-profiles-helpers.js';

test('a profile edited in Settings is what a spawn by it runs, and a worker at the limit cannot spawn', async ({ harness, page }) => {
  test.setTimeout(180_000);
  await runAgentProfiles(harness, page, 'desktop');
});
