// The assistant's agent-thread companion pane under the compact layout
// (docs/specs/standing-agents.md, "Agents segment"), on both mock
// providers: a tap on an Agents
// tray row reveals the worker's companion on the phone with its header and
// Open as Pane in view, and its own Stop interrupts the worker.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { agentRow, mockInterrupt, openAgents, seedLeadWithWorker } from './agents-tray-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';

const LEAD = 'Release lead';
const WORKER = 'Changelog worker';

function companion(page: Page) {
  return page.locator('section[data-pane-kind="agent-thread"]');
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: compact: a tray row reveals the worker in a companion that fits the phone`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const { workerPath, token, worker } = await seedLeadWithWorker(harness, { tag: `compact-assistant-companion-${provider}`, provider, lead: LEAD, worker: WORKER });

    await harness.open(page);
    await openAgents(page, LEAD);
    await agentRow(page, token).getByTestId('agent-tray-row-open').tap();
    await expect(companion(page).locator(`[data-ui-surface="chat"][data-thread-id="${worker}"]`)).toBeVisible();
    const title = companion(page).getByTestId('chat-header-title');
    await expect(title).toHaveText(WORKER);
    await expect(title).toBeInViewport();
    const openAsPane = companion(page).getByTestId('agent-thread-open-as-pane');
    await expect(openAsPane).toBeInViewport({ ratio: 1 });
    const interrupt = companion(page).getByTestId('composer-interrupt');
    await expect(interrupt).toBeInViewport({ ratio: 1 });
    await captureAgentMessage(page, `${provider}-compact-assistant-companion.png`, companion(page).getByTestId('chat-header-title-row'), interrupt);

    const stopped = mockInterrupt(harness, workerPath);
    await interrupt.tap();
    await stopped;
    await expect(companion(page).getByTestId('composer-send')).toBeVisible();
  });
}
