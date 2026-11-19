// Owned workers in the sidebar on the desktop layout
// (docs/specs/standing-agents.md, "Sidebar", ruling 36). The body is
// owned-workers-sidebar-helpers.ts.
import { test } from './fixtures.js';
import { runOwnedWorkersSidebar } from './owned-workers-sidebar-helpers.js';

test('owned workers have no row, placed ones do, and the spawner counts them', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runOwnedWorkersSidebar(harness, page, 'desktop');
});
