// Deleting a thread deletes its owned workers, on the desktop layout
// (docs/specs/standing-agents.md, "Owned workers"). The body is
// owned-workers-delete-helpers.ts.
import { test } from './fixtures.js';
import { runOwnedWorkersDelete } from './owned-workers-delete-helpers.js';

test('deleting a thread deletes its workers and stops the running one', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runOwnedWorkersDelete(harness, page, 'desktop');
});
