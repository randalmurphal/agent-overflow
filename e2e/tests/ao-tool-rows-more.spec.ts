// The AO tool rows ao-tool-rows.spec.ts does not cover, on both mock
// providers (docs/specs/agent-thread-tools.md, How calls surface in the
// timeline): every other thread tool, the workflow run and memory tools,
// the browser tools on a local page, the remote tools on a paired computer,
// and a reply row in the thread that answered. Each row's header and body
// read as decoded text, never the JSON the model read. Screenshots of
// every row collapsed and expanded go to the test's output directory.

import { test } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { driveMoreAoToolRows, readMoreRows } from './ao-tool-rows-more-helpers.js';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: every other AO tool row reads as decoded text`, async ({ page }) => {
    test.setTimeout(300_000);
    let app: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [app, remote] = await startTogether(launchHarness(), launchHarness());
      await driveMoreAoToolRows(app, remote, provider, `desktop-${provider}`);
      await readMoreRows(app, page, (name) => test.info().outputPath(`${provider}-more-${name}.png`));
    } finally {
      await Promise.allSettled([app?.close(), remote?.close()]);
    }
  });
}
