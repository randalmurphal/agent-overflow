// Spawn placement on the desktop layout (docs/specs/standing-agents.md
// ruling 36): an assistant spawns a nested, a grouped and a standalone
// thread, each answers it and is marked read, the sidebar shows each where
// its placement puts it, and the row menu and a drag onto a group move them
// between placements. The body is standing-agent-spawn-placement-helpers.ts.
import { test } from './fixtures.js';
import { runSpawnPlacement } from './standing-agent-spawn-placement-helpers.js';

test('an assistant places what it spawns, and the menu and a drag move it', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runSpawnPlacement(harness, page, 'desktop');
});
