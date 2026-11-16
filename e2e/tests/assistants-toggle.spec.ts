// Turning assistants off and on (docs/specs/standing-agents.md, "Turning
// assistants off"), on both mock providers: with the Assistants setting off
// in Settings, the Assistants section, the assistant's row and New
// Assistant in the project's menu are gone and the assistant's loop does
// not fire though it comes due; on again, they are back and the loop serves
// its missed fires once, saying how many. A pane already showing the
// assistant's thread stays readable with a line saying assistants are off
// in place of its composer and without New Thread, Same Assistant. The
// body is assistants-toggle-helpers.ts.
import { test } from './fixtures.js';
import { runAssistantsToggle, runAssistantsTogglePane } from './assistants-toggle-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: assistants turned off are hidden and paused, and come back when turned on`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    await runAssistantsToggle(harness, page, 'desktop', provider);
  });

  test(`${provider}: an assistant's open pane says assistants are off and takes nothing`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    await runAssistantsTogglePane(harness, page, 'desktop', provider);
  });
}
