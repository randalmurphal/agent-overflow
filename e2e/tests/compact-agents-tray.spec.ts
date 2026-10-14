// The activity rail's Agents segment under the compact layout
// (docs/specs/standing-agents.md, "Agents segment"), on both mock
// providers: the toggle sits in the rail beside the other segments within
// a phone's width; running, blocked and nested rows fit it, the title on
// its own line above the state, elapsed time and touch-sized actions; a
// row's digest opens in place; and a tap on Stop cascades as a click does
// on desktop.
import type { Locator } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { awaitGate, type ProviderName } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { CHILD, LEAD, agentRow, agentsBody, approvalTool, ledgerRow, mockInterrupt, openAgents, seedAgentsTree, startTree } from './agents-tray-helpers.js';

/** The row stays inside the window, its title above its controls, and its actions are touch-sized. */
async function expectFits(row: Locator, viewportWidth: number): Promise<void> {
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
  expect(await row.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  const title = (await row.getByTestId('agent-tray-row-title').boundingBox())!;
  const controls = (await row.getByTestId('agent-tray-row-controls').boundingBox())!;
  expect(controls.y, 'controls below the title').toBeGreaterThanOrEqual(title.y + title.height - 1);
  for (const id of ['agent-tray-row-stop', 'agent-tray-row-details']) {
    const button = (await row.getByTestId(id).boundingBox())!;
    expect(button.height, id).toBeGreaterThanOrEqual(36);
    expect(button.x + button.width, id).toBeLessThanOrEqual(viewportWidth);
  }
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: Agents rows fit a phone, open their digest in place and stop on a tap`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const tree = await seedAgentsTree(harness, provider, `compact-agents-${provider}`);
    const started = await startTree(harness, tree);
    await awaitGate(harness, 'child-hold', tree.childPath);
    await harness.open(page);
    await openAgents(page, LEAD);
    const width = page.viewportSize()!.width;

    // The toggle holds its place in the rail at a phone's width.
    const rail = page.getByTestId('activity-rail').filter({ visible: true }).locator('[data-activity-rail-row]');
    expect(await rail.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    const toggle = page.getByTestId('activity-rail-agents-toggle').filter({ visible: true });
    const toggleBox = (await toggle.boundingBox())!;
    expect(toggleBox.x + toggleBox.width).toBeLessThanOrEqual(width);

    const child = agentRow(page, started.childToken);
    const grand = agentRow(page, started.grandToken);
    const send = agentRow(page, started.sendToken);
    for (const row of [child, grand, send]) {
      await expect(row).toBeVisible();
      await expectFits(row, width);
    }
    await expect(grand).toHaveAttribute('data-depth', '1');
    await expect(grand.getByTestId('agent-tray-row-blocked')).toContainText(`Needs approval for ${approvalTool(provider)}`);
    await capture(page, `agents-${provider}-compact-rows.png`, agentsBody(page));

    // The digest opens in place under the row.
    await child.getByTestId('agent-tray-row-details').tap();
    const digest = child.getByTestId('agent-tray-row-digest');
    await expect(digest).toBeVisible();
    const digestBox = (await digest.boundingBox())!;
    expect(digestBox.x + digestBox.width).toBeLessThanOrEqual(width);
    await capture(page, `agents-${provider}-compact-digest.png`, child);

    // A tap on Stop cascades into the child's work.
    const childInterrupted = mockInterrupt(harness, tree.childPath);
    await child.getByTestId('agent-tray-row-stop').tap();
    await expect(page.getByTestId('toast-message').filter({ hasText: `Stopped ${CHILD}` }))
      .toHaveText(`Stopped ${CHILD}: its turn, 1 background task and 1 agent of its own.`, { timeout: 30_000 });
    await childInterrupted;
    expect(ledgerRow(harness, started.childToken)!.state).toBe('cancelled');
    expect(ledgerRow(harness, started.grandToken)!.state).toBe('cancelled');
  });
}
