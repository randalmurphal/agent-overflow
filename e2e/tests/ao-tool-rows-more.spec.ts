// The AO tool rows ao-tool-rows.spec.ts does not cover, on the Claude mock
// (docs/specs/agent-thread-tools.md, How calls surface in the timeline):
// every other thread tool, the workflow run and memory tools, the browser
// tools on a local page, the remote tools on a paired computer, and a reply
// row in the thread that answered. Each row's header and body read as
// decoded text, never the JSON the model read, on a desktop page and then,
// over the same history, on a phone page (the compact project's device),
// where each row also stays inside the phone-width thread screen. With
// AO_E2E_SCREENSHOTS=1, screenshots of every row collapsed and expanded go
// to the test's output directory.
//
// One provider is enough: both stamp the same meta.mcp on a row (Claude's
// parse_assistant_test.go TestParseAssistant_MCPToolUseNormalizesName,
// Codex's protocol_test.go TestClassifyNotification_McpToolCallEnrichesToolMeta
// and ...CompletionCarriesResultContent), and the result decoding and the
// rows above it branch on no provider. ao-tool-rows.spec.ts keeps the
// thread and workflow servers' Codex wiring end to end and
// browser-tools.spec.ts the browser server's; the remote server's Codex
// registration is internal/app TestRemoteMCPRequiresEnabledPairedComputer.

import { devices } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { driveMoreAoToolRows, readMoreRows } from './ao-tool-rows-more-helpers.js';

test('claude: every other AO tool row reads as decoded text, on a desktop and on a phone', async ({ browser, page }) => {
  test.setTimeout(300_000);
  let app: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  try {
    [app, remote] = await startTogether(launchHarness(), launchHarness());
    await driveMoreAoToolRows(app, remote, 'claude', 'claude');
    await readMoreRows(app, page, (name) => test.info().outputPath(`claude-more-${name}.png`));

    const phoneContext = await browser.newContext({ ...devices['Pixel 7'], colorScheme: 'dark' });
    try {
      const phone = await phoneContext.newPage();
      const viewport = phone.viewportSize()!;
      await readMoreRows(app, phone, (name) => test.info().outputPath(`compact-claude-more-${name}.png`), async (row) => {
        const box = (await row.boundingBox())!;
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
      });
    } finally {
      await phoneContext.close();
    }
  } finally {
    await Promise.allSettled([app?.close(), remote?.close()]);
  }
});
