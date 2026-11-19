// Deleting a thread deletes its owned workers, under the compact layout
// (docs/specs/standing-agents.md, "Owned workers"). The body is
// owned-workers-delete-helpers.ts.
import { test } from './fixtures.js';
import { runOwnedWorkersDelete } from './owned-workers-delete-helpers.js';

test('deleting a thread deletes its workers and stops the running one on a phone', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runOwnedWorkersDelete(harness, page, 'compact');
});
