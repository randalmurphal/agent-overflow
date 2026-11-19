// Agent profiles and the sub-thread limit under the compact layout
// (docs/specs/agent-thread-tools.md, "Agent profiles" and "Sub-thread
// depth"). The body is agent-profiles-helpers.ts.
import { test } from './fixtures.js';
import { runAgentProfiles } from './agent-profiles-helpers.js';

test('a profile edited in Settings is what a spawn by it runs, and a worker at the limit cannot spawn, on a phone', async ({ harness, page }) => {
  test.setTimeout(180_000);
  await runAgentProfiles(harness, page, 'compact');
});
