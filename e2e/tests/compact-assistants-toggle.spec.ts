// Turning assistants off and on under the compact layout
// (docs/specs/standing-agents.md, "Turning assistants off"): the setting
// turned from Settings on a phone hides the Assistants section and New
// Assistant in the project's long-press menu and pauses the loop; on again,
// they are back and the loop serves its missed fires once. The thread
// screen of an assistant says assistants are off in place of its composer.
// The body is assistants-toggle-helpers.ts.
import { test } from './fixtures.js';
import { runAssistantsToggle, runAssistantsTogglePane } from './assistants-toggle-helpers.js';

test('compact: assistants turned off are hidden and paused, and come back when turned on', async ({ harness, page }) => {
  test.setTimeout(180_000);
  await runAssistantsToggle(harness, page, 'compact', 'claude');
});

test("compact: an assistant's open thread screen says assistants are off and takes nothing", async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runAssistantsTogglePane(harness, page, 'compact', 'claude');
});
