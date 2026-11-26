// The Issues pane's Dashboards screen (docs/specs/issues.md, "Dashboards"),
// shared by the desktop and compact projects, through the shipped UI, the
// real backend and the fake Jira, whose five finished sprints, active
// sprint and histories the board syncs.
//
// Covered on both: the tab opens the board's built-in dashboard, counted
// from the board's copy once it has synced; the pace says why it is not
// compared when the copy holds too few earlier sprints, and with This
// sprint shows its change from the last sprint colored as bad news; a
// widget that counts now shows "Now" and no change mark; a bar opens its
// tickets, 100 a page, and the pages move; Team to Mine narrows every
// number to the person's tickets; a person in the table narrows the page
// with a chip that clears; a ticket's history reaches the count; a
// change made in Jira reaches the open dashboard with no Refresh; a
// chart's value shows on hover (on compact, on focus); the grid lays out
// again when the pane is resized; a listed ticket opens. On desktop: four
// opens and closes of the screen leave the page's heap and DOM as they
// were.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { connectFakeJiraBoard, openIssuesPane, press, renameJiraIssue, type ConnectedBoard, type ExtraJira } from './tracker-helpers.js';

export interface DashboardsSurface {
  compact: boolean;
}

const DAY = 24 * 3600_000;
const iso = (offset: number) => new Date(Date.now() + offset).toISOString();
const POINTS = 'customfield_10016';

let changeId = 30000;
const status = (at: number, from: [string, string], to: [string, string]) => ({
  id: String((changeId += 1)), author: 'acc-user1', created: iso(at),
  items: [{ fieldId: 'status', field: 'status', fieldtype: 'jira', from: from[0], fromString: from[1], to: to[0], toString: to[1] }],
});
const TODO: [string, string] = ['10000', 'To Do'];
const DOING: [string, string] = ['10001', 'In Progress'];
const DONE: [string, string] = ['10002', 'Done'];

// Five finished sprints, A to E, two weeks each, E completed two days
// ago when Sprint 1 (the seed's) started. A to D each finished one ticket
// of 4 points, User 1's; E finished 105 of 1 point, the even ones User
// 1's (53) and the odd User 2's (52). So the pace over the last 5 is
// (16 + 105) / 5 = 24.2 points, Mine's (16 + 53) / 5 = 13.8 and User
// 2's 52 / 5 = 10.4. Sprint 1 holds the seed's WEB-1 and WEB-2 (no
// estimate, nobody's), WEB-20 (User 2, 3 points, in progress for 6
// days: stuck), WEB-21 (User 1, 2 points, done yesterday) and WEB-22
// (User 1, 5 points, to do): This sprint's pace is 2 points, down 103
// from Sprint E's 105.
const SPRINT_E = 15;
const DASHBOARDS: ExtraJira = {
  projects: [],
  boards: [],
  estimation: POINTS,
  assignable: ['acc-user1', 'acc-user2'],
  users: [{ accountId: 'acc-user2', displayName: 'User 2', email: 'user2@example.com', timeZone: 'UTC' }],
  fields: [
    { id: POINTS, name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } },
  ],
  sprints: ['A', 'B', 'C', 'D', 'E'].map((name, i) => {
    const end = -(2 + 14 * (4 - i)) * DAY;
    return { id: 11 + i, name: `Sprint ${name}`, state: 'closed' as const, start: iso(end - 14 * DAY), end: iso(end), complete: iso(end) };
  }),
  issues: [
    ...[11, 12, 13, 14].map((sprint) => ({
      id: String(10400 + sprint), key: `WEB-${400 + sprint}`, project: 'WEB', type: '10002', status: '10002',
      summary: `Done in sprint ${sprint}`, sprints: [sprint], assignee: 'acc-user1', created: iso(-80 * DAY), values: { [POINTS]: 4 },
    })),
    ...Array.from({ length: 105 }, (_, i) => ({
      id: String(10200 + i), key: `WEB-${200 + i}`, project: 'WEB', type: '10002', status: '10002',
      summary: `Finished in Sprint E, ${i + 1}`, sprints: [SPRINT_E], assignee: i % 2 === 0 ? 'acc-user1' : 'acc-user2',
      created: iso(-15 * DAY), values: { [POINTS]: 1 },
    })),
    {
      id: '10120', key: 'WEB-20', project: 'WEB', type: '10002', status: '10001', summary: 'Stuck in progress', sprints: [1],
      assignee: 'acc-user2', created: iso(-10 * DAY), values: { [POINTS]: 3 }, changelog: [status(-6 * DAY, TODO, DOING)],
    },
    {
      id: '10121', key: 'WEB-21', project: 'WEB', type: '10002', status: '10002', summary: 'Done yesterday', sprints: [1],
      assignee: 'acc-user1', created: iso(-10 * DAY), values: { [POINTS]: 2 },
      changelog: [status(-5 * DAY, TODO, DOING), status(-1 * DAY, DOING, DONE)],
    },
    {
      id: '10122', key: 'WEB-22', project: 'WEB', type: '10002', status: '10000', summary: 'Waiting for User 1', sprints: [1],
      assignee: 'acc-user1', created: iso(-10 * DAY), values: { [POINTS]: 5 },
    },
  ],
};

function widget(screen: Locator, id: string): Locator {
  return screen.locator(`[data-testid="dashboard-widget"][data-widget="${id}"]`);
}

/** Switches the pane to Dashboards: its tab, or the folded picker. */
async function openDashboards(page: Page, pane: Locator, compact: boolean): Promise<Locator> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Dashboards' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="dashboards"]'), compact);
  }
  const screen = pane.getByTestId('dashboard-screen');
  await expect(screen).toBeVisible({ timeout: 15_000 });
  return screen;
}

async function pickRange(page: Page, screen: Locator, id: string, compact: boolean): Promise<void> {
  await press(screen.getByTestId('dashboard-range'), compact);
  await press(page.getByTestId(`dashboard-range-${id}`), compact);
  await expect(screen.getByTestId('dashboard-counting')).toHaveCount(0, { timeout: 15_000 });
}

async function spans(screen: Locator): Promise<Record<string, number>> {
  return screen.getByTestId('dashboard-cell-span').evaluateAll((els) => Object.fromEntries(els.map((el) => [
    el.querySelector<HTMLElement>('[data-testid="dashboard-widget"]')?.dataset.widget ?? '',
    Number((el as HTMLElement).dataset.span),
  ])));
}

async function openBoardDashboards(harness: HarnessApp, page: Page, name: string, compact: boolean): Promise<{ board: ConnectedBoard; pane: Locator; screen: Locator }> {
  const board = await connectFakeJiraBoard(harness, name, DASHBOARDS);
  await harness.open(page);
  const pane = await openIssuesPane(page, compact);
  await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible({ timeout: 30_000 });
  const screen = await openDashboards(page, pane, compact);
  // The board's copy has synced its sprints and histories: the pace is counted over all five sprints.
  await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 60_000 });
  await expect(widget(screen, 'stuck').getByTestId('dashboard-number-value')).toHaveText('1', { timeout: 60_000 });
  return { board, pane, screen };
}

export function registerDashboardsFlow({ compact }: DashboardsSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-dashboards-${s}`;

  test('Dashboards counts the board, compares a range with the one before, and narrows to a person', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { board, pane, screen } = await openBoardDashboards(harness, page, name('count'), compact);
    await expect(screen.getByTestId('dashboard-title')).toHaveText('Overview');
    await expect(screen.getByTestId('dashboard-range')).toHaveText('Last 5 sprints');
    await expect(screen.getByTestId('dashboard-periods')).toHaveText('Sprint A to Sprint E');

    // The pace has no five sprints before these to compare with, and says so.
    const pace = widget(screen, 'pace');
    await expect(pace.getByTestId('dashboard-change-gap')).toHaveText('Not compared: This computer keeps fewer than 5 finished sprints before these to compare with.');
    await expect(pace.getByTestId('dashboard-change')).toHaveCount(0);
    // A number that counts now has no change mark.
    const stuck = widget(screen, 'stuck');
    await expect(stuck.getByTestId('dashboard-now')).toBeVisible();
    await expect(stuck.getByTestId('dashboard-change')).toHaveCount(0);
    await expect(stuck.getByTestId('dashboard-change-gap')).toHaveCount(0);
    await expect(widget(screen, 'stuck_list').getByTestId('dashboard-list-row')).toHaveText([/WEB-20\s*Stuck in progress/]);
    await expect(widget(screen, 'finished').getByTestId('dashboard-bar')).toHaveCount(5);

    // This sprint against the last: down 103 points, bad news for a pace.
    await pickRange(page, screen, 'this_sprint', compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('2');
    // WEB-21's history reached the copy: its start to done is counted.
    await expect(widget(screen, 'start_to_done').getByTestId('dashboard-number-value')).not.toHaveText('None', { timeout: 30_000 });
    await expect(pace.getByTestId('dashboard-change')).toHaveText('Down 103 points from 105, the last sprint');
    await expect(pace.getByTestId('dashboard-change')).toHaveAttribute('data-tone', 'bad');
    await pickRange(page, screen, 'sprints_5', compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2');

    // Team to Mine: every number is User 1's.
    const whose = screen.getByRole('radiogroup', { name: 'Whose work' });
    await press(whose.getByRole('radio', { name: 'Mine' }), compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('13.8', { timeout: 15_000 });
    await expect(stuck.getByTestId('dashboard-number-value')).toHaveText('0');
    const carrying = widget(screen, 'carrying');
    await expect(carrying.getByTestId('dashboard-table-label')).toHaveText(['User 1']);
    await press(whose.getByRole('radio', { name: 'Team' }), compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });
    await expect(carrying.getByTestId('dashboard-table-label')).toHaveText(['User 1', 'User 2']);

    // A person in the table narrows the page, and the chip clears it.
    await press(carrying.getByTestId('dashboard-table-label').filter({ hasText: 'User 2' }), compact);
    const chip = screen.getByTestId('dashboard-chip');
    await expect(chip).toHaveText('User 2');
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('10.4', { timeout: 15_000 });
    await expect(stuck.getByTestId('dashboard-number-value')).toHaveText('1');
    await press(chip.getByTestId('dashboard-chip-clear'), compact);
    await expect(chip).toHaveCount(0);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });

    // A change in Jira reaches the open dashboard with no Refresh: the
    // board keeps syncing under it, and its change counts again.
    await renameJiraIssue(board, 'WEB-20', 'Stuck in progress, renamed in Jira');
    await expect(widget(screen, 'stuck_list').getByTestId('dashboard-list-row')).toHaveText([/WEB-20\s*Stuck in progress, renamed in Jira/], { timeout: 30_000 });

    // A listed ticket opens.
    await press(widget(screen, 'stuck_list').getByTestId('dashboard-list-row'), compact);
    await expect(pane.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10120', { timeout: 15_000 });
  });

  // The heap is Chromium's, read the same on either layout: desktop only.
  if (!compact) {
    test('Dashboards gives back its heap and DOM when the screen closes', async ({ harness, page, context }) => {
      test.setTimeout(180_000);
      const { pane } = await openBoardDashboards(harness, page, name('memory'), compact);
      const cdp = await context.newCDPSession(page);
      // What the page holds: the heap snapshot's objects, less the code V8
      // compiles as functions grow hot, and the pane strip's DOM.
      const measure = async () => {
        await cdp.send('HeapProfiler.collectGarbage');
        const chunks: string[] = [];
        const onChunk = (e: { chunk: string }) => { chunks.push(e.chunk); };
        cdp.on('HeapProfiler.addHeapSnapshotChunk', onChunk);
        await cdp.send('HeapProfiler.takeHeapSnapshot', { reportProgress: false });
        cdp.off('HeapProfiler.addHeapSnapshotChunk', onChunk);
        const snap = JSON.parse(chunks.join('')) as { snapshot: { meta: { node_fields: string[]; node_types: [string[]] } }; nodes: number[] };
        const fields = snap.snapshot.meta.node_fields;
        const typeAt = fields.indexOf('type');
        const sizeAt = fields.indexOf('self_size');
        const code = snap.snapshot.meta.node_types[0].indexOf('code');
        let held = 0;
        for (let i = 0; i < snap.nodes.length; i += fields.length) if (snap.nodes[i + typeAt] !== code) held += snap.nodes[i + sizeAt];
        const nodes = await page.evaluate(() => document.querySelector('[data-testid="pane-strip"]')!.getElementsByTagName('*').length);
        return { heldKB: Math.round(held / 1024), nodes };
      };
      const toBoard = async () => {
        await pane.locator('[data-testid="issues-tab"][data-screen="board"]').click();
        await expect(pane.getByTestId('dashboard-screen')).toHaveCount(0);
        await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible();
      };
      const openAndClose = async () => {
        const screen = await openDashboards(page, pane, false);
        await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });
        await expect(screen.getByTestId('dashboard-counting')).toHaveCount(0);
        const open = await measure();
        await toBoard();
        return open;
      };
      // The screen's code loads and each widget draws once; then four
      // opens and closes.
      await toBoard();
      await openAndClose();
      const before = await measure();
      const opens = [];
      for (let i = 0; i < 4; i += 1) opens.push(await openAndClose());
      const after = await measure();
      const cost = `before ${JSON.stringify(before)}; each open ${JSON.stringify(opens)}; after four closed ${JSON.stringify(after)}`;
      test.info().annotations.push({ type: 'dashboards screen cost', description: cost });
      console.log(`dashboards screen cost: ${cost}`);
      expect(after.nodes).toBe(before.nodes);
      for (const open of opens) expect(open.nodes).toBeGreaterThan(before.nodes);
      // Four screens come and go and the page holds what it held before,
      // give or take less than half of what one open screen holds.
      const oneScreen = Math.min(...opens.map((open) => open.heldKB - before.heldKB));
      expect(after.heldKB - before.heldKB).toBeLessThan(oneScreen / 2);
      await cdp.detach();
    });
  }

  test('Dashboards opens a bar\'s tickets by the page, shows a chart\'s values, and lays out again with the pane', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { screen } = await openBoardDashboards(harness, page, name('drill'), compact);
    const finished = widget(screen, 'finished');
    const sprintE = finished.locator(`[data-testid="dashboard-bar"][data-bucket="sprint:${SPRINT_E}"]`);

    // A bar's value on hover, or on focus where there is no hover.
    if (compact) await sprintE.focus();
    else await sprintE.hover();
    await expect(finished.getByTestId('dashboard-hover')).toHaveText('Sprint E, 105 points');

    // The bar's tickets, 100 a page.
    await press(sprintE, compact);
    const drill = page.getByTestId('dashboard-drill');
    await expect(drill).toBeVisible();
    await expect(page.getByRole('dialog', { name: 'Finished per sprint, Sprint E' })).toBeVisible();
    await expect(drill.getByTestId('dashboard-drill-value')).toHaveText('105 points');
    await expect(drill.getByTestId('dashboard-drill-count')).toHaveText('105 tickets');
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(100);
    await expect(drill.getByTestId('dashboard-drill-page')).toHaveText('Page 1 of 2');
    await press(drill.getByTestId('dashboard-drill-next'), compact);
    await expect(drill.getByTestId('dashboard-drill-page')).toHaveText('Page 2 of 2');
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(5);
    await press(drill.getByTestId('dashboard-drill-prev'), compact);
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(100);
    await page.keyboard.press('Escape');
    await expect(drill).toHaveCount(0);

    // The grid lays out to the pane's width.
    const viewport = page.viewportSize()!;
    if (compact) {
      expect(await spans(screen)).toMatchObject({ pace: 6, stuck: 6, finished: 12, carrying: 12, stuck_list: 12 });
      const before = await finished.locator('svg').first().getAttribute('width');
      await page.setViewportSize({ width: viewport.width + 60, height: viewport.height });
      await expect.poll(async () => finished.locator('svg').first().getAttribute('width')).not.toBe(before);
    } else {
      await page.setViewportSize({ width: 1800, height: 1000 });
      await expect.poll(async () => (await spans(screen)).pace).toBe(4);
      expect(await spans(screen)).toMatchObject({ finished: 8, carrying: 4, stuck_list: 6, open_by_epic: 6 });
      await page.setViewportSize({ width: 1000, height: 1000 });
      await expect.poll(async () => (await spans(screen)).pace).not.toBe(4);
      const narrower = await spans(screen);
      expect(narrower.finished).toBe(12);
      expect([6, 8, 12]).toContain(narrower.pace);
    }
  });
}
