// The Dashboards screen's agent widgets (docs/specs/issues.md,
// "Dashboards"), shared by the desktop and compact projects, through the
// shipped UI, the real backend, the fake Jira and a mock provider whose
// session makes REAL calls to the tracker tools' dashboard_widget.
//
// Covered on both: Ask your agent opens a new thread in the board's
// project with an unsent draft naming the board; the agent reads the
// rules, a query on a column the board's copy lacks is refused with a
// sentence naming it, and two widgets it shows appear above the grid with
// their numbers and How this is counted, their query among it; Dismiss
// drops one and Keep puts the other on a new dashboard, which the picker
// lists after Overview and another browser shows; edit mode removes the
// widget, and with it the dashboard, which takes both browsers back to
// Overview. Number proposals two to a row keep their buttons inside their
// cards, and Keep's new-dashboard name takes every key typed beside the
// dashboards it lists. A count whose unit is estimate draws, and drills,
// as tickets on a board estimated in points. With neither Claude nor Codex
// turned on, Ask your agent is disabled and says why. A board on a paired computer takes a
// widget its agent proposed from this computer's screen, the kept
// dashboard is still there after that computer restarts, and Ask your
// agent from this computer's screen opens its thread on that computer.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Browser, Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { setScenario, threadRows, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { connectFakeJira, JIRA_SITE, openIssuesPane, press, seedFakeJira, type ExtraJira } from './tracker-helpers.js';

export interface DashboardAgentSurface {
  compact: boolean;
}

const SERVER = 'ao-tracker-tools';
const SEED = `About the dashboards of the board WEB board (${JIRA_SITE}/1): `;

/** WEB-1 (in progress), WEB-2 and WEB-3 (to do): every ticket of the
 * seed's board is open. */
const OPEN = {
  title: 'Open tickets', words: ['Tickets the board counts as not done.'], sql: 'SELECT id AS ticket FROM tickets WHERE open = 1',
  reduce: 'count', unit: 'tickets', form: 'number',
};
const BY_GROUP = {
  title: 'By status group', words: ['Tickets by their status group.'], sql: 'SELECT id AS ticket, status_group AS bucket FROM tickets',
  reduce: 'count', unit: 'tickets', bucket: 'text', form: 'bars',
};
const PRIORITY = { ...OPEN, title: 'Urgent', sql: "SELECT id AS ticket FROM tickets WHERE priority = 'High'" };

/** The seed's board estimated in story points. */
const POINTS_BOARD: ExtraJira = {
  projects: [],
  boards: [],
  estimation: 'customfield_10016',
  fields: [{ id: 'customfield_10016', name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } }],
};

function proposal(screen: Locator, title: string): Locator {
  return screen.locator('[data-testid="dashboard-widget"]').filter({ has: screen.page().getByTestId('dashboard-proposal') }).filter({ hasText: title });
}

function kept(screen: Locator, title: string): Locator {
  return screen.locator('[data-testid="dashboard-widget"]').filter({ hasNot: screen.page().getByTestId('dashboard-proposal') }).filter({ hasText: title });
}

/** Switches the pane to Dashboards and waits for Overview's numbers. */
async function openDashboards(page: Page, pane: Locator, compact: boolean): Promise<Locator> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Dashboards' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="dashboards"]'), compact);
  }
  const screen = pane.getByTestId('dashboard-screen');
  await expect(screen.getByTestId('dashboard-title')).toHaveText('Overview', { timeout: 30_000 });
  return screen;
}

async function issuesDashboards(page: Page, compact: boolean): Promise<Locator> {
  const pane = await openIssuesPane(page, compact);
  await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible({ timeout: 60_000 });
  return openDashboards(page, pane, compact);
}

/** Back to the Issues pane from the thread Ask opened: beside it on the
 * desktop, through the list on compact. */
async function backToDashboards(page: Page, compact: boolean): Promise<Locator> {
  if (compact) {
    await page.getByTestId('chat-header-title-row').getByTestId('compact-back').tap();
    await openIssuesPane(page, compact);
  }
  return page.getByTestId('issues-pane').getByTestId('dashboard-screen');
}

async function pickDashboard(page: Page, screen: Locator, title: string, compact: boolean): Promise<void> {
  await press(screen.getByTestId('dashboard-picker'), compact);
  await press(page.getByRole('menuitemcheckbox', { name: title }), compact);
  await expect(screen.getByTestId('dashboard-title')).toHaveText(title);
}

/** The thread's turn: the rules, a refused query, and two widgets shown. */
function widgetScenario(name: string, provider: ProviderName): Record<string, unknown> {
  return threadToolsScenario({
    name,
    provider,
    echoUserMessage: true,
    turns: [{
      steps: [
        { call: { server: SERVER, tool: 'dashboard_widget', args: {} } },
        { call: { server: SERVER, tool: 'dashboard_widget', args: { widget: PRIORITY, show: true } } },
        { call: { server: SERVER, tool: 'dashboard_widget', args: { widget: OPEN, show: true } } },
        { call: { server: SERVER, tool: 'dashboard_widget', args: { widget: BY_GROUP, show: true } } },
      ],
      text: 'Two widgets are on the dashboard for you to keep.',
    }],
  });
}

/** The four answers of widgetScenario's turn, as the model received them. */
async function widgetAnswers(harness: HarnessApp, cwd: string): Promise<Array<{ isError: boolean; text: string }>> {
  const out = [];
  for (let i = 0; i < 4; i++) {
    const event = await harness.awaitMcpResult({ server: SERVER, tool: 'dashboard_widget', cwd, timeoutMs: 60_000 });
    out.push({ isError: Boolean(event.report.isError), text: event.report.result ?? '' });
  }
  return out;
}

function expectWidgetAnswers(answers: Array<{ isError: boolean; text: string }>): void {
  const [rules, refused, open, byGroup] = answers;
  expect(rules.isError, rules.text).toBe(false);
  expect(JSON.parse(rules.text)).toContain('CREATE TABLE main.tickets(');
  expect(refused.isError).toBe(true);
  expect(refused.text).toMatch(/^\[dashboard_widget_refused\] This widget's query failed: no such column: priority\. Change its query\.$/);
  expect(open.isError, open.text).toBe(false);
  expect(JSON.parse(open.text)).toMatchObject({
    board: 'WEB board', value: 3, rows: 3, shown: "It is on the board's dashboards for the user to keep or dismiss.",
  });
  expect(byGroup.isError, byGroup.text).toBe(false);
  expect(JSON.parse(byGroup.text)).toMatchObject({ buckets: [{ label: 'todo', total: 2 }, { label: 'in_progress', total: 1 }] });
}

/** A project listing the seed's board, with its path for the scenario. */
async function boardProject(harness: HarnessApp, name: string, threads: ReadonlyArray<Record<string, unknown>> = [], extra?: ExtraJira): Promise<{ projectId: string; path: string; threadIds: string[] }> {
  await seedFakeJira(harness, extra);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {}, threads }] });
  const project = seed.projects[0];
  await connectFakeJira(harness, project.projectId);
  return { projectId: project.projectId, path: project.path, threadIds: project.threadIds };
}

async function otherBrowser(browser: Browser, harness: HarnessApp, compact: boolean): Promise<{ page: Page; close: () => Promise<void> }> {
  const context = await browser.newContext(compact ? { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } : {});
  const page = await context.newPage();
  await harness.open(page);
  return { page, close: () => context.close() };
}

export function registerDashboardAgentFlow({ compact }: DashboardAgentSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-dashboards-agent-${s}`;

  test('Ask your agent proposes widgets that Keep puts on a new dashboard another browser shows, and edit mode removes', async ({ harness, page, browser }) => {
    test.setTimeout(240_000);
    const project = await boardProject(harness, name('ask'));
    await harness.open(page);
    let screen = await issuesDashboards(page, compact);
    await expect(screen.getByTestId('dashboard-edit')).toHaveCount(0);

    // Ask your agent: a new thread in the board's project, its draft
    // unsent and naming the board.
    await press(screen.getByTestId('dashboard-ask'), compact);
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    const input = page.getByLabel('Message Input').filter({ visible: true });
    await expect(input).toHaveValue(SEED, { timeout: 30_000 });
    await expect.poll(async () => (await threadRows(harness)).filter((r) => r.projectId === project.projectId).length, { timeout: 30_000 }).toBe(1);
    const thread = (await threadRows(harness)).find((r) => r.projectId === project.projectId)!;
    await setScenario(harness, project.path, widgetScenario(name('ask'), thread.provider as ProviderName));
    await input.press('End');
    await input.pressSequentially('how many tickets are open?');
    await press(page.getByTestId('composer-send').filter({ visible: true }), compact);
    expectWidgetAnswers(await widgetAnswers(harness, project.path));

    // The two widgets shown, with their numbers and how each is counted.
    screen = await backToDashboards(page, compact);
    await expect(screen.getByTestId('dashboard-proposals')).toBeVisible({ timeout: 30_000 });
    const open = proposal(screen, 'Open tickets');
    await expect(open.getByTestId('dashboard-number-value')).toHaveText('3');
    await expect(proposal(screen, 'By status group').locator('[data-testid="dashboard-widget-error"]')).toHaveCount(0);
    await press(open.getByTestId('dashboard-how-toggle'), compact);
    await expect(open.getByTestId('dashboard-how')).toContainText('Tickets the board counts as not done.');
    await expect(open.getByTestId('dashboard-how-sql')).toHaveText(OPEN.sql);
    await press(open.getByTestId('dashboard-how-toggle'), compact);

    // Dismiss drops one; Keep from Overview asks where, and a new
    // dashboard takes it.
    await press(proposal(screen, 'By status group').getByTestId('dashboard-dismiss'), compact);
    await expect(proposal(screen, 'By status group')).toHaveCount(0);
    await press(open.getByTestId('dashboard-keep'), compact);
    await page.getByTestId('dashboard-keep-new-title').fill('Agent widgets');
    await press(page.getByTestId('dashboard-keep-new'), compact);
    await expect(screen.getByTestId('dashboard-title')).toHaveText('Agent widgets', { timeout: 30_000 });
    await expect(screen.getByTestId('dashboard-proposals')).toHaveCount(0);
    await expect(kept(screen, 'Open tickets').getByTestId('dashboard-number-value')).toHaveText('3', { timeout: 30_000 });
    await press(screen.getByTestId('dashboard-picker'), compact);
    await expect(page.getByRole('menuitemcheckbox')).toHaveText([/^\s*Overview\s*$/, /^\s*Agent widgets\s*✓\s*$/]);
    await page.keyboard.press('Escape');

    // Another browser lists it, and follows the edits live.
    const other = await otherBrowser(browser, harness, compact);
    try {
      const otherScreen = await issuesDashboards(other.page, compact);
      await pickDashboard(other.page, otherScreen, 'Agent widgets', compact);
      await expect(kept(otherScreen, 'Open tickets').getByTestId('dashboard-number-value')).toHaveText('3', { timeout: 30_000 });

      await press(screen.getByTestId('dashboard-edit'), compact);
      // Removing its last widget removes the dashboard.
      await press(kept(screen, 'Open tickets').getByTestId('dashboard-remove'), compact);
      await expect(screen.getByTestId('dashboard-title')).toHaveText('Overview', { timeout: 30_000 });
      await expect(otherScreen.getByTestId('dashboard-title')).toHaveText('Overview', { timeout: 30_000 });
    } finally {
      await other.close();
    }
  });

  test('number proposals side by side keep their buttons in their cards, a count in estimates draws tickets on a points board, a new dashboard\'s name takes every key, and Ask needs a provider with the tracker tools', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const provider: ProviderName = 'claude';
    const project = await boardProject(harness, name('numbers'), [{ title: 'Board questions', provider }], POINTS_BOARD);
    // All tickets counts with the unit estimate: a count is of tickets
    // whatever its unit says.
    const numbers = [OPEN, { ...OPEN, title: 'Closed tickets', sql: 'SELECT id AS ticket FROM tickets WHERE open = 0' }, { ...OPEN, title: 'All tickets', sql: 'SELECT id AS ticket FROM tickets', unit: 'estimate' }];
    await setScenario(harness, project.path, threadToolsScenario({
      name: name('numbers'),
      provider,
      turns: [{ steps: numbers.map((widget) => ({ call: { server: SERVER, tool: 'dashboard_widget', args: { widget, show: true } } })), text: 'Three numbers.' }],
    }));
    await harness.open(page);
    const screen = await issuesDashboards(page, compact);
    await harness.send(project.threadIds[0], 'three numbers');
    for (const _ of numbers) {
      const event = await harness.awaitMcpResult({ server: SERVER, tool: 'dashboard_widget', cwd: project.path, timeoutMs: 60_000 });
      expect(event.report.isError, event.report.result).toBeFalsy();
    }
    const all = proposal(screen, 'All tickets');
    await expect(all.getByTestId('dashboard-number-value')).toHaveText('3', { timeout: 30_000 });
    await expect(all.getByTestId('dashboard-number')).toHaveAttribute('aria-label', 'All tickets: 3 tickets. Show its tickets');
    await press(all.getByTestId('dashboard-number'), compact);
    await expect(page.getByTestId('dashboard-drill-value')).toHaveText('3 tickets');
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('dashboard-drill')).toHaveCount(0);

    // Each proposal's buttons sit inside its own card, two to a row on
    // compact.
    const cards = screen.locator('[data-testid="dashboard-widget"]').filter({ has: page.getByTestId('dashboard-proposal') });
    await expect(cards).toHaveCount(3);
    const boxes = [];
    for (const card of await cards.all()) {
      const box = (await card.boundingBox())!;
      boxes.push(box);
      for (const id of ['dashboard-proposal-thread', 'dashboard-dismiss', 'dashboard-keep', 'dashboard-how-toggle']) {
        const b = (await card.getByTestId(id).boundingBox())!;
        expect(b.x, `${id} starts inside its card`).toBeGreaterThanOrEqual(box.x);
        expect(b.x + b.width, `${id} ends inside its card`).toBeLessThanOrEqual(box.x + box.width + 0.5);
      }
    }
    if (compact) expect(Math.abs(boxes[0].y - boxes[1].y)).toBeLessThan(1);

    // With a dashboard kept, Keep… from Overview lists it above the name
    // field, and the name typed there takes every key.
    await press(proposal(screen, 'Open tickets').getByTestId('dashboard-keep'), compact);
    await page.getByTestId('dashboard-keep-new-title').fill('First');
    await press(page.getByTestId('dashboard-keep-new'), compact);
    await expect(screen.getByTestId('dashboard-title')).toHaveText('First', { timeout: 30_000 });
    await pickDashboard(page, screen, 'Overview', compact);
    await press(proposal(screen, 'Closed tickets').getByTestId('dashboard-keep'), compact);
    await expect(page.getByRole('menuitem', { name: 'Keep on First' })).toBeVisible();
    const title = page.getByTestId('dashboard-keep-new-title');
    await press(title, compact);
    await title.pressSequentially('Backlog jk');
    await expect(title).toHaveValue('Backlog jk');
    await title.press('Enter');
    await expect(screen.getByTestId('dashboard-title')).toHaveText('Backlog jk', { timeout: 30_000 });

    // With no provider whose agents get the tracker tools turned on, Ask
    // says which to turn on.
    await expect(screen.getByTestId('dashboard-ask')).toBeEnabled();
    await harness.rpc('UpdateSettings', { claudeEnabled: false, codexEnabled: false });
    await expect(screen.getByTestId('dashboard-ask')).toBeDisabled();
    await expect(screen.getByTestId('dashboard-ask')).toHaveAttribute('title', 'Ask your agent needs a provider whose agents get the tracker tools, turned on in Settings and installed on this computer.');
  });

  test('a paired computer\'s board takes the widget its agent proposed, and keeps it through that computer\'s restart', async ({ page }) => {
    test.setTimeout(300_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-issues-dashboards-'));
    const dataDir = join(root, 'state');
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir }));
      const laptop = await pairOwnDevice(home, remote, 'Laptop');
      const provider: ProviderName = 'claude';
      const project = await boardProject(remote, name('paired'), [{ title: 'Board questions', provider }]);
      await setScenario(remote, project.path, widgetScenario(name('paired'), provider));
      await home.open(page);
      const screen = await issuesDashboards(page, compact);
      await remote.send(project.threadIds[0], 'how many tickets are open?');
      expectWidgetAnswers(await widgetAnswers(remote, project.path));

      const open = proposal(screen, 'Open tickets');
      await expect(open.getByTestId('dashboard-number-value')).toHaveText('3', { timeout: 30_000 });
      await press(open.getByTestId('dashboard-keep'), compact);
      await page.getByTestId('dashboard-keep-new-title').fill('Laptop widgets');
      await press(page.getByTestId('dashboard-keep-new'), compact);
      await expect(screen.getByTestId('dashboard-title')).toHaveText('Laptop widgets', { timeout: 30_000 });
      // The other proposal is the board's computer's memory alone.
      await expect(proposal(screen, 'By status group')).toHaveCount(1);

      // Restarted, at its new address, with its Jira served again: the
      // kept dashboard is there and the proposal is not.
      expect(await remote.stop()).toBe(true);
      remote = await launchHarness({ dataDir });
      await seedFakeJira(remote);
      await home.rpc('RepairBackendAddress', laptop, `https://127.0.0.1:${remote.bootstrap.port}`);
      // The open screen reads the board again once the computer is back,
      // still on the dashboard picked.
      await expect(screen.getByTestId('dashboard-title')).toHaveText('Laptop widgets', { timeout: 60_000 });
      await expect(kept(screen, 'Open tickets').getByTestId('dashboard-number-value')).toHaveText('3', { timeout: 60_000 });
      await expect(screen.getByTestId('dashboard-proposals')).toHaveCount(0);

      // Ask your agent from this computer's screen opens the thread on the
      // board's computer, in the board's project.
      await press(screen.getByTestId('dashboard-ask'), compact);
      await expect(page.getByLabel('Message Input').filter({ visible: true })).toHaveValue(SEED, { timeout: 30_000 });
      const board = remote;
      await expect.poll(async () => (await threadRows(board)).filter((r) => r.projectId === project.projectId).length, { timeout: 30_000 }).toBe(2);
    } finally {
      await remote?.close();
      await home?.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}
