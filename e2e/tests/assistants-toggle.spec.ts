// Turning assistants off and on (docs/specs/standing-agents.md, "Turning
// assistants off"), on the Claude mock: with the Assistants setting off
// in Settings, the Assistants section, the assistant's row and New
// Assistant in the project's menu are gone and the assistant's loop does
// not fire though it comes due; on again, they are back and the loop serves
// its missed fires once, saying how many. A pane already showing the
// assistant's thread stays readable with a line saying assistants are off
// in place of its composer and without New Thread, Same Assistant, and an
// edit open in it closes, its text kept as the composer's draft. The body
// is assistants-toggle-helpers.ts; two computers each with their own
// setting are assistants-toggle-paired.spec.ts. The setting and the paused
// loop are app state that branches on no provider
// (internal/app/app_assistants_switch.go); a wake's delivery on Codex is
// the standing-agent-wake-* specs'.
import { test } from './fixtures.js';
import { runAssistantsToggle, runAssistantsTogglePane } from './assistants-toggle-helpers.js';

test('claude: assistants turned off are hidden and paused, and come back when turned on', async ({ harness, page }) => {
  test.setTimeout(180_000);
  await runAssistantsToggle(harness, page, 'desktop', 'claude');
});

test("claude: an assistant's open pane says assistants are off and takes nothing", async ({ harness, page }) => {
  test.setTimeout(120_000);
  await runAssistantsTogglePane(harness, page, 'desktop', 'claude');
});
