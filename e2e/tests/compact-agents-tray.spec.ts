// The activity rail's Agents segment under the compact layout
// (docs/specs/standing-agents.md, "Agents segment"), on the Claude mock
// (the layout is the same on both; each provider's rows and Stop are
// agents-tray.spec.ts's): the toggle sits in the rail beside the other segments within
// a phone's width; running, blocked and nested rows fit it, the title on
// its own line above the state, elapsed time and touch-sized actions; a
// row's digest opens in place; a tap on Stop cascades as a click does
// on desktop; and the stopped row leaves for Past, whose rows fit the same
// way with touch-sized Open in Agent Pane and Show in Sidebar. Failed reads
// of the list and of Past fit with touch-sized Retry, which reads them; an
// ask's Past row is the thread asked with its provider; a running ask's tap
// opens its copy with a composer that takes no message, no pickers, no
// attach, no edit or fork on its messages and no Keep, and
// once answered the copy's pane goes, its Past row's digest carries the
// answer and its icon offers the thread asked; an assistant's helper row, open and past, has Open in
// Agent Pane and no Show in Sidebar; and Past's count sits beside its label
// and its touch-sized Show more and Show less page it and go back to the
// first page.
import type { Locator } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { advanceGate, awaitGate } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import {
  ASKED,
  ASK_ANSWER,
  CHILD,
  HELPER,
  LEAD,
  agentRow,
  agentsBody,
  approvalTool,
  companionPane,
  expectAskCopyReadOnly,
  expectPastCountBesideLabel,
  failAgentReads,
  ledgerRow,
  mockInterrupt,
  openAgentRow,
  openAgents,
  openPast,
  pastList,
  pastRow,
  seedAgentsTree,
  seedAssistantWithHelper,
  seedPastWithAsk,
  seedSettledWorkers,
  startTree,
} from './agents-tray-helpers.js';

/** The row stays inside the window, its title above its controls, and its actions are touch-sized. */
async function expectFits(row: Locator, viewportWidth: number, buttons = ['agent-tray-row-stop', 'agent-tray-row-details', 'agent-tray-row-open-pane']): Promise<void> {
  const box = (await row.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
  expect(await row.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  const title = (await row.getByTestId('agent-tray-row-title').boundingBox())!;
  const controls = (await row.getByTestId('agent-tray-row-controls').boundingBox())!;
  expect(controls.y, 'controls below the title').toBeGreaterThanOrEqual(title.y + title.height - 1);
  for (const id of buttons) {
    const button = (await row.getByTestId(id).boundingBox())!;
    expect(button.height, id).toBeGreaterThanOrEqual(36);
    expect(button.x + button.width, id).toBeLessThanOrEqual(viewportWidth);
  }
}

/** A text control inside the window with a touch-sized height. */
async function expectTouchSized(button: Locator, viewportWidth: number): Promise<void> {
  const box = (await button.boundingBox())!;
  expect(box.height, 'touch-sized').toBeGreaterThanOrEqual(36);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
}

test(`claude: Agents rows fit a phone, open their digest in place and stop on a tap`, async ({ harness, page }) => {
  const provider = 'claude';
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

  // The stopped child leaves the open rows for Past.
  await expect(openAgentRow(page, started.childToken)).toHaveCount(0, { timeout: 30_000 });
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('1');
  await openPast(page);
  const past = pastRow(page, started.childToken);
  await expect(past.getByTestId('agent-tray-row-state')).toHaveText('stopped');
  await expect(past.getByTestId('agent-tray-row-stop')).toHaveCount(0);
  await expectFits(past, width, ['agent-tray-row-details', 'agent-tray-row-open-pane', 'agent-tray-row-show-in-sidebar']);
  await capture(page, `agents-${provider}-compact-past.png`, agentsBody(page));
});

test(`claude: a failed read fits a phone with a touch-sized Retry, and Past then shows an ask as the thread asked`, async ({ harness, page }) => {
  const provider = 'claude';
  test.setTimeout(240_000);
  const { askToken } = await seedPastWithAsk(harness, provider, `compact-agents-fail-${provider}`);
  const reads = await failAgentReads(page);
  await harness.open(page);
  await openAgents(page, LEAD);
  const width = page.viewportSize()!.width;
  const body = agentsBody(page);
  const error = body.getByTestId('activity-rail-agents-error');
  await expect(error.getByTestId('row-error-msg')).toContainText('Could not read agents:', { timeout: 30_000 });
  const pastError = body.getByTestId('activity-rail-agents-past-error');
  await expect(pastError).toBeVisible();
  for (const [region, retry] of [[error, 'activity-rail-agents-error-retry'], [pastError, 'activity-rail-agents-past-error-retry']] as const) {
    const box = (await region.getByTestId(retry).boundingBox())!;
    expect(box.height, retry).toBeGreaterThanOrEqual(36);
    expect(box.x + box.width, retry).toBeLessThanOrEqual(width);
  }
  await capture(page, `agents-${provider}-compact-read-failed.png`, body);

  reads.failing = false;
  await error.getByTestId('activity-rail-agents-error-retry').tap();
  await pastError.getByTestId('activity-rail-agents-past-error-retry').tap();
  await expect(error).toHaveCount(0);
  await expect(pastError).toHaveCount(0);
  await openPast(page);
  const ask = pastRow(page, askToken);
  await expect(ask.getByTestId('agent-tray-row-title')).toHaveText(ASKED);
  await expect(ask.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
  await expectFits(ask, width, ['agent-tray-row-details', 'agent-tray-row-open-pane']);
  await capture(page, `agents-${provider}-compact-ask.png`, body);
});

test(`claude: a running ask opens its read-only copy on a phone, and once answered shows the answer and offers the thread asked`, async ({ harness, page }) => {
  const provider = 'claude';
  test.setTimeout(240_000);
  const { askToken, copyThreadId, askedPath } = await seedPastWithAsk(harness, provider, `compact-agents-ask-${provider}`, true);
  const hold = await awaitGate(harness, 'ask-hold', askedPath);
  await harness.open(page);
  await openAgents(page, LEAD);
  const width = page.viewportSize()!.width;
  const row = openAgentRow(page, askToken);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running', { timeout: 30_000 });
  await expectFits(row, width, ['agent-tray-row-stop', 'agent-tray-row-open-pane']);
  await row.getByTestId('agent-tray-row-open').tap();
  const copyPane = companionPane(page);
  await expect(copyPane.getByTestId('chat-header-title')).toHaveText(`Ask: ${ASKED}`, { timeout: 30_000 });
  const input = copyPane.getByTestId('composer-root').locator('textarea');
  await expect(input).toBeDisabled();
  await expect(input).toHaveAttribute('placeholder', "An agent's question runs in this copy. It takes no messages.");
  await expect(copyPane.getByTestId('side-chat-keep')).toHaveCount(0);
  await expectAskCopyReadOnly(copyPane);
  await capture(page, `agents-${provider}-compact-ask-copy.png`, copyPane);

  // Answered: the copy and its pane go, and Past's row shows the answer
  // on a tap and offers the thread asked as that.
  await advanceGate(harness, hold.mockId, 'ask-hold');
  await expect(copyPane).toHaveCount(0, { timeout: 30_000 });
  await expect.poll(async () => (await harness.rpc<{ id?: string } | null>('GetThread', copyThreadId).catch(() => null))?.id ?? '', { timeout: 30_000 }).toBe('');
  await openPast(page);
  const past = pastRow(page, askToken);
  await expect(past.getByTestId('agent-tray-row-open-pane')).toHaveAttribute('aria-label', `Open ${ASKED}, the thread asked, in Agent Pane`);
  await past.getByTestId('agent-tray-row-details').tap();
  await expect(past.getByTestId('agent-tray-row-digest')).toContainText(ASK_ANSWER);
  await expectFits(past, width, ['agent-tray-row-details', 'agent-tray-row-open-pane']);
  await capture(page, `agents-${provider}-compact-ask-answered.png`, agentsBody(page));
});

test(`claude: an assistant's helper row fits a phone with Open in Agent Pane and no Show in Sidebar`, async ({ harness, page }) => {
  const provider = 'claude';
  test.setTimeout(240_000);
  const { home, token } = await seedAssistantWithHelper(harness, provider, `compact-agents-helper-${provider}`);
  const hold = await awaitGate(harness, 'helper-hold', home);
  await harness.open(page);
  await openAgents(page, LEAD);
  const width = page.viewportSize()!.width;
  const row = openAgentRow(page, token);
  await expect(row.getByTestId('agent-tray-row-title')).toHaveText(HELPER, { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-show-in-sidebar')).toHaveCount(0);
  await expectFits(row, width, ['agent-tray-row-stop', 'agent-tray-row-open-pane']);
  await capture(page, `agents-${provider}-compact-helper.png`, agentsBody(page));
  await advanceGate(harness, hold.mockId, 'helper-hold');
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('1', { timeout: 30_000 });
  await openPast(page);
  const past = pastRow(page, token);
  await expect(past.getByTestId('agent-tray-row-show-in-sidebar')).toHaveCount(0);
  await expectFits(past, width, ['agent-tray-row-open-pane']);
});

test(`claude: Past's count sits beside its label, and Show more and Show less are touch-sized and page it`, async ({ harness, page }) => {
  const provider = 'claude';
  test.setTimeout(240_000);
  await seedSettledWorkers(harness, provider, `compact-agents-paging-${provider}`, 9);
  await harness.open(page);
  await openAgents(page, LEAD);
  const width = page.viewportSize()!.width;
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('9', { timeout: 30_000 });
  await expectPastCountBesideLabel(page);
  const rows = (await openPast(page)).getByTestId('agent-tray-row');
  await expect(rows).toHaveCount(8);
  const more = agentsBody(page).getByTestId('activity-rail-agents-past-more');
  const less = agentsBody(page).getByTestId('activity-rail-agents-past-less');
  await expect(more).toHaveText('Show 1 more…');
  await expect(less).toHaveCount(0);
  await expectTouchSized(more, width);
  await more.tap();
  await expect(rows).toHaveCount(9);
  await expect(more).toHaveCount(0);
  await expectTouchSized(less, width);
  await capture(page, `agents-${provider}-compact-past-paged.png`, agentsBody(page));
  await less.tap();
  await expect(pastList(page).getByTestId('agent-tray-row')).toHaveCount(8);
  await expect(less).toHaveCount(0);
  await expect(more).toHaveText('Show 1 more…');
});
