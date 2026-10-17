// Spawn placement under the compact layout (docs/specs/standing-agents.md
// ruling 36): the same three placements as the desktop spec, with every
// move made from the row's menu button, since compact rows do not drag.
// The body is standing-agent-spawn-placement-helpers.ts.
import { test } from './fixtures.js';
import { runSpawnPlacement } from './standing-agent-spawn-placement-helpers.js';

test('an assistant places what it spawns, and the menu moves it on a phone', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runSpawnPlacement(harness, page, 'compact');
});
