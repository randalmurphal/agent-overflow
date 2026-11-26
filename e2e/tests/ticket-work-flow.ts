// A ticket's Work, Ask and Start work (docs/specs/issues.md, "Ticket" and
// "Start work applies per-project defaults"), shared by the desktop and
// compact projects, through the shipped UI, the real backend, the fake
// Jira and the fake forge. Covered: a pull request whose title names a
// ticket carries a chip on the Pull requests page, from the list the real
// pump stored, and the chip opens the ticket; the ticket's Work (in Details
// on compact) lists the thread linked to it and that pull request; Ask
// opens a thread linked to the ticket with its file attached to the
// unsent draft and writes nothing to the tracker, in the project whose
// board the ticket was opened from when two list it; Start work shows the
// project's defaults, makes the thread in a worktree on the branch named
// after the ticket, moves and assigns the ticket once each, saves the
// defaults when asked and opens the thread; a move the tracker refuses
// leaves the thread made and unopened with the refusal on screen, and Try
// again sends only the move and opens that same thread; a move made from
// the Status field after that refusal shows its Saving… under the field
// and nothing under the Start work row; while the run's move is out, its
// row says Saving…, the assignment's says Waiting., and the thread's names
// the project; a card's menu has Ask, which makes the thread in the
// board's project and loads Work's store only when chosen, and Start
// work…, which opens the ticket with its form, whose run shows in the
// ticket's Work, also when the ticket was read before it opens (on compact
// in the Details it opens); a Backlog row's menu has the same Ask and
// Start work…. Work's forge search lists, once each, what names the key
// by title, by the start of its branch and by a branch the forge or this
// computer has, on GitHub and GitLab, a stored row with its CI, and on
// Refresh the forge's change; a failed search says its kind, a rate
// limit with when to retry and Retry, and a login to fix with none.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  clearForgeRefusal, forgeInvocations, publishPullRequest, refuseForgeRoute, seedForge, setForgeOffline, setForgeRateLimit,
  type ForgeHTTPInvocation, type ForgePull, type ForgeRepo,
} from './forge-helpers.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { launchPullsHarness, openPage, pickFilter, row as pullRow } from './pulls-page-flow.js';
import { confirmOnHost, methodIdByName, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { recordOpens } from './preview-gateway-helpers.js';
import { longPress } from './touch-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';
import { BOARD_ID, JIRA_SITE, connectFakeJira, connectFakeJiraBoard, openIssuesPane, press, seedFakeJira, trackerRequests } from './tracker-helpers.js';
import { openBacklog } from './issues-backlog-flow.js';

/** Settles with whether the first answer to a `SubscribeTrackerTicket`
 * on this page's next socket carried the ticket's detail. */
function subscribeAnswered(page: Page): Promise<boolean> {
  const method = methodIdByName('SubscribeTrackerTicket');
  return new Promise((resolve) => {
    page.once('websocket', (socket) => {
      const asked = new Set<string>();
      socket.on('framesent', ({ payload }) => {
        if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
        const frame = JSON.parse(payload) as { id?: string; methodId?: number };
        if (frame.methodId === method && frame.id) asked.add(frame.id);
      });
      socket.on('framereceived', ({ payload }) => {
        if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
        const frame = JSON.parse(payload) as { id?: string; result?: { detail?: unknown } };
        if (frame.id && asked.has(frame.id)) resolve(!!frame.result?.detail);
      });
    });
  });
}

export interface WorkSurface {
  compact: boolean;
}

// WEB-1 "First story" (in progress) and WEB-2 "Second task" (to do) in
// the seed's sprint.
const WEB1 = '10101';
const WEB2 = '10102';
const MOVE_ROUTE = 'POST rest/api/3/issue/{issue}/transitions';
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';

interface ThreadRow {
  id: string;
  projectId: string;
  title: string;
  worktreePath?: string;
  ticketLink?: { key: string; origin: string } | null;
}

const threadRows = (harness: HarnessApp) => harness.rpc<ThreadRow[]>('HarnessListThreadRows');

/** Error toasts only: the class is the toast's one type discriminator. */
const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** Opens the card's ticket and answers its Work, opening Details on a narrow
 * ticket. On compact the board shows one column, picked from its strip. */
async function openWork(page: Page, issueId: string, compact: boolean, column?: string): Promise<{ ticket: Locator; work: Locator }> {
  const pane = await openIssuesPane(page, compact);
  if (column) await showColumn(pane, column, compact);
  await expect(card(pane, issueId)).toBeVisible({ timeout: 30_000 });
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  return { ticket, work: await workOf(ticket, compact) };
}

/** The Work section on the ticket's rail; a narrow ticket (always on
 * compact, and on desktop beside a second pane) opens it from Details once
 * the ticket is read. */
async function workOf(ticket: Locator, compact: boolean): Promise<Locator> {
  const details = ticket.getByTestId('ticket-details');
  const work = ticket.getByTestId('ticket-rail').getByTestId('ticket-work');
  await expect(work.or(details).first()).toBeVisible({ timeout: 30_000 });
  if (compact) await expect(details).toBeVisible();
  if ((await details.isVisible()) && (await details.getAttribute('aria-expanded')) !== 'true') await press(details, compact);
  await expect(work).toBeVisible();
  return work;
}

/** A Start work run's row of `name`. */
function step(work: Locator, name: string): Locator {
  return work.getByTestId('start-work-run').locator(`[data-testid="start-work-step"][data-step="${name}"]`);
}

/** Opens card `issueId`'s menu (a right click, a long press on the phone)
 * and its Show more. */
async function cardMenuMore(page: Page, pane: Locator, issueId: string, key: string, compact: boolean): Promise<Locator> {
  const menu = page.getByRole('menu', { name: `Card actions for ${key}` });
  const target = card(pane, issueId).getByTestId('issues-card-open');
  await expect(target).toBeVisible({ timeout: 30_000 });
  if (compact) await longPress(page, target, menu);
  else await target.click({ button: 'right' });
  await expect(menu).toBeVisible();
  await press(menu.getByTestId('action-menu-more'), compact);
  return menu;
}

/** Opens the page on the Backlog screen and WEB-3's row menu (in the
 * backlog: a right click, its menu button on the phone) and its Show more. */
async function backlogRowMenuMore(harness: HarnessApp, page: Page, compact: boolean): Promise<Locator> {
  await harness.open(page);
  const pane = await openIssuesPane(page, compact);
  await expect(pane.getByTestId('issues-card').first()).toBeVisible({ timeout: 30_000 });
  const backlog = await openBacklog(page, pane, compact);
  const target = backlog.locator('[data-testid="issues-backlog-row"][data-issue="10103"]');
  await expect(target).toBeVisible({ timeout: 30_000 });
  const menu = page.getByRole('menu', { name: 'Card actions for WEB-3' });
  if (compact) await press(target.getByTestId('issues-backlog-row-menu'), true);
  else await target.getByTestId('issues-backlog-row-open').click({ button: 'right' });
  await expect(menu).toBeVisible();
  await press(menu.getByTestId('action-menu-more'), compact);
  return menu;
}

/** Shows column `name` on compact (the strip picks it); a no-op on desktop. */
async function showColumn(pane: Locator, name: string, compact: boolean): Promise<void> {
  if (!compact) return;
  const tab = pane.getByTestId('issues-column-tab').filter({ hasText: name });
  await expect(tab).toBeVisible({ timeout: 30_000 });
  await tab.tap();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
}

/** The thread the page shows; on compact the thread screen. */
async function expectThreadShown(page: Page, title: string, compact: boolean): Promise<void> {
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('chat-header-title')).toHaveText(title, { timeout: 30_000 });
}

// The forge's pull requests for WEB-1's search: 501 and 504 name it in the
// title, 502 in a branch that begins with it, 503 in a branch of this
// computer's that names it later; 505 (WEB-12), 506 ("web 1") and 507
// (the key in its body) only seem to.
const SEARCH_PULLS: ForgePull[] = [
  { number: 501, title: 'WEB-1 Retry the cursor read', author: 'alice', authorName: 'Alice Ng', headRef: 'retry-cursor' },
  { number: 502, title: 'Cache the session', author: 'bob', authorName: 'Bob Li', headRef: 'WEB-1-cache', state: 'merged' },
  { number: 503, title: 'Move the host', author: 'carol', headRef: 'feature/WEB-1-host', state: 'closed' },
  { number: 504, title: 'WEB-1 Draft of the docs', author: 'alice', authorName: 'Alice Ng', headRef: 'docs', draft: true },
  { number: 505, title: 'WEB-12 Other story', author: 'alice', headRef: 'other' },
  { number: 506, title: 'Move web 1 to the new host', author: 'alice', headRef: 'move' },
  { number: 507, title: 'Unrelated', body: 'Follows WEB-1.', author: 'alice', headRef: 'unrelated' },
];

/** A project that lists the WEB board, a checkout of `repo` on the fake
 * `forge` with branches feature/WEB-1-host and web-1-new, and the forge
 * seeded with `pulls`. */
async function searchProject(
  harness: HarnessApp, name: string, pulls: ForgePull[] = SEARCH_PULLS, forge: 'github' | 'gitlab' = 'github',
): Promise<{ projectId: string; repo: string }> {
  await seedFakeJira(harness);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: { commits: [{ message: 'init', files: { 'README.md': '# Web\n' } }] } }] });
  const { projectId, path } = seed.projects[0];
  await connectFakeJira(harness, projectId);
  const repo = `ao-e2e/${name}-${Date.now().toString(36)}`;
  await seedForge(harness, [{ forge, project: repo, pulls }]);
  harnessGit(harness, path, 'branch', 'feature/WEB-1-host');
  harnessGit(harness, path, 'branch', 'web-1-new');
  harnessGit(harness, path, 'remote', 'add', 'origin', `git://${forge}.com/${repo}.git`);
  return { projectId, repo };
}

// The fake GitLab's merge requests for WEB-1's search: 701 names it in
// the title, 702 only in a branch this computer does not have, which the
// forge keeps; 704 (WEB-12) only seems to.
const GITLAB_SEARCH_PULLS: ForgePull[] = [
  { number: 701, title: 'WEB-1 Retry the cursor read', author: 'alice', authorName: 'Alice Ng', headRef: 'retry-cursor' },
  { number: 702, title: 'Fix the cursor', author: 'bob', authorName: 'Bob Li', headRef: 'feature/web-1-cursor', state: 'merged' },
  { number: 704, title: 'WEB-12 Other story', author: 'alice', headRef: 'web-12-other' },
];

/** The fake forge's answered requests of GraphQL operation `op`. */
async function forgeOps(harness: HarnessApp, op: string): Promise<ForgeHTTPInvocation[]> {
  return (await forgeInvocations(harness)).filter((c): c is ForgeHTTPInvocation => c.via === 'http' && c.operation === op);
}

/** Work's pull request rows as `#number state`, in order. */
async function pullRows(work: Locator): Promise<string[]> {
  const rows = work.getByTestId('ticket-work-pull');
  const out: string[] = [];
  for (let i = 0; i < await rows.count(); i += 1) {
    const row = rows.nth(i);
    out.push(`${(await row.locator('.tabular-nums').textContent())?.trim()} ${await row.getAttribute('data-state')}`);
  }
  return out;
}

/** The URLs this page asks its computer to open (OpenExternalURL). */
function hostOpens(page: Page): () => readonly string[] {
  const method = methodIdByName('OpenExternalURL');
  const urls: string[] = [];
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const frame = JSON.parse(payload) as { methodId?: number; params?: unknown[] };
      if (frame.methodId === method && typeof frame.params?.[0] === 'string') urls.push(frame.params[0]);
    });
  });
  return () => urls;
}

export function registerTicketWorkFlow({ compact }: WorkSurface): void {
  test('Work lists the pull requests the forge finds by the ticket\'s key, once each, and none that only seems to name it', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const { repo } = await searchProject(harness, compact ? 'compact-work-search' : 'work-search');
    const opens = hostOpens(page);
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB1, compact);
    // 501 and 504 by their titles, 502 by its branch's start, 503 by this
    // computer's branch; the rest only seem to name WEB-1.
    await expect.poll(() => pullRows(work), { timeout: 30_000 }).toEqual(['#504 draft', '#501 open', '#502 merged', '#503 closed']);
    await expect(work.getByTestId('ticket-work-searching')).toHaveCount(0);
    const rows = work.getByTestId('ticket-work-pull');
    await expect(rows.nth(0).getByTestId('ticket-work-pull-state')).toHaveText('Draft');
    await expect(rows.nth(1)).toContainText('WEB-1 Retry the cursor read');
    await expect(rows.nth(1)).toContainText('Alice Ng');
    await expect(rows.nth(2).getByTestId('ticket-work-pull-state')).toHaveText('Merged');
    await expect(rows.nth(2)).toContainText('Bob Li');
    await expect(rows.nth(3).getByTestId('ticket-work-pull-state')).toHaveText('Closed');
    // The branches the found pull requests name fold into them.
    await expect(work.getByTestId('ticket-work-branch')).toHaveCount(1);
    await expect(work.getByTestId('ticket-work-branch')).toContainText('web-1-new');
    await expect(work.getByTestId('ticket-work-forge-problem')).toHaveCount(0);

    // One key search per read, when Work shows and on Refresh, and
    // Refresh shows what the forge holds now: 501 merged since.
    expect(await forgeOps(harness, 'PullsByKey')).toHaveLength(1);
    await seedForge(harness, [{ forge: 'github', project: repo, pulls: SEARCH_PULLS.map((p) => (p.number === 501 ? { ...p, state: 'merged' } : p)) }]);
    await press(ticket.getByTestId('ticket-refresh'), compact);
    await expect.poll(async () => (await forgeOps(harness, 'PullsByKey')).length, { timeout: 30_000 }).toBe(2);
    await expect.poll(() => pullRows(work), { timeout: 30_000 }).toEqual(['#504 draft', '#501 merged', '#502 merged', '#503 closed']);

    // A found row opens on the forge, as every pull request row does.
    await press(rows.nth(2), compact);
    await expect.poll(opens).toEqual([`https://github.com/${repo}/pull/502`]);
  });

  test('a pull request the Pull requests page stored and the forge search finds is listed once, with its CI', async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchPullsHarness();
    try {
      await seedFakeJira(harness);
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: compact ? 'compact-work-once' : 'work-once', repo: { commits: [{ message: 'init', files: { 'README.md': '# Web\n' } }] } }],
      });
      const { projectId, path } = seed.projects[0];
      await connectFakeJira(harness, projectId);
      const repo: ForgeRepo = {
        forge: 'github',
        project: `ao-e2e/work-once-${compact ? 'compact' : 'desktop'}-${Date.now().toString(36)}`,
        pulls: [
          { number: 601, title: 'WEB-1 Retry the cursor read', author: 'alice', authorName: 'Alice Ng', ci: { jobs: [{ name: 'unit', status: 'success' }] } },
          { number: 602, title: 'WEB-1 First try', author: 'alice', headRef: 'first-try', state: 'merged' },
        ],
      };
      await publishPullRequest(harness, path, repo, { 'src/cursor.ts': 'export const cursor = 1;\n' });
      await harness.open(page);
      // The Pull requests page stores the open pull request.
      const pulls = await openPage(page);
      await pickFilter(page, pulls, 'all');
      await expect(pullRow(page, 601)).toBeVisible({ timeout: 30_000 });
      if (compact) {
        await page.getByTestId('compact-back').tap();
        await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
      }
      const { work } = await openWork(page, WEB1, compact);
      await expect.poll(async () => (await forgeOps(harness, 'PullsByKey')).length, { timeout: 30_000 }).toBe(1);
      await expect(work.getByTestId('ticket-work-searching')).toHaveCount(0, { timeout: 30_000 });
      // 601 once, the stored row with its CI; 602 only the search finds.
      await expect.poll(() => pullRows(work)).toEqual(['#601 open', '#602 merged']);
      await expect(work.getByTestId('ticket-work-pull').nth(0)).toContainText('CI passed');
    } finally {
      await harness.close();
    }
  });

  test('a failed search says its kind in the Pull requests page\'s words, and the branches still show', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await searchProject(harness, compact ? 'compact-work-search-fails' : 'work-search-fails');
    await refuseForgeRoute(harness, {
      route: 'gh graphql PullsByKey', status: 403,
      body: { message: 'Resource not accessible by personal access token', documentation_url: 'https://docs.github.com/rest' },
    });
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB1, compact);
    const problem = work.getByTestId('pulls-project-error');
    await expect(problem).toHaveAttribute('data-error-kind', 'forbidden', { timeout: 30_000 });
    await expect(problem).toContainText('Could not finish searching ao-e2e/');
    await expect(work.getByTestId('ticket-work-branch')).toHaveCount(2);
    await expect(work.getByTestId('pulls-project-retry')).toHaveCount(0);

    // The forge off the network: a failure the next read may not have, with Retry.
    await clearForgeRefusal(harness, 'gh graphql PullsByKey');
    await setForgeOffline(harness, true);
    await press(ticket.getByTestId('ticket-refresh'), compact);
    await expect(problem).toHaveAttribute('data-error-kind', 'transient', { timeout: 60_000 });
    await expect(problem).toContainText('failed to refresh from the forge (id: ');
    await expect(work.getByTestId('ticket-work-branch')).toHaveCount(2);

    // A rate limit says when Retry can read again, and offers it.
    await setForgeOffline(harness, false);
    await setForgeRateLimit(harness, { forge: 'github', pool: 'graphql', remaining: 0, reset: Math.floor(Date.now() / 1000) + 3600 });
    await press(problem.getByTestId('pulls-project-retry'), compact);
    await expect(problem).toHaveAttribute('data-error-kind', 'rate_limited', { timeout: 30_000 });
    await expect(problem).toContainText(/GitHub rate limit reached, so the search of ao-e2e\/\S+ did not finish\. Retry after /);
    await expect(problem.getByTestId('pulls-project-retry')).toBeVisible();
    await expect(work.getByTestId('ticket-work-branch')).toHaveCount(2);
  });

  test('on GitLab, Work lists a merge request whose key is only in a branch this computer lacks, and a login to fix reads as setup', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await searchProject(harness, compact ? 'compact-work-search-gitlab' : 'work-search-gitlab', GITLAB_SEARCH_PULLS, 'gitlab');
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB1, compact);
    // GitLab's rows only: a stored list an earlier test's project left
    // stays until the Pull requests page's next read prunes it.
    const gitlabRows = async () => (await pullRows(work)).filter((row) => row.startsWith('!'));
    await expect.poll(gitlabRows, { timeout: 30_000 }).toEqual(['!701 open', '!702 merged']);
    await expect(work.getByTestId('ticket-work-searching')).toHaveCount(0);
    await expect(work.getByTestId('ticket-work-pull').filter({ hasText: '!702' })).toContainText('Fix the cursor');
    await expect(work.getByTestId('ticket-work-forge-problem')).toHaveCount(0);

    // The forge refuses the login every time: a setup failure, with no
    // Retry, and the branches still show.
    await refuseForgeRoute(harness, { route: 'glab api merge request list', status: 401, body: { message: '401 Unauthorized' } });
    await press(ticket.getByTestId('ticket-refresh'), compact);
    const problem = work.getByTestId('pulls-project-error');
    await expect(problem).toHaveAttribute('data-error-kind', 'setup', { timeout: 30_000 });
    await expect(problem).toContainText('Could not finish searching ao-e2e/');
    await expect(work.getByTestId('pulls-project-retry')).toHaveCount(0);
    await expect(work.getByTestId('ticket-work-branch')).toHaveCount(2);
  });

  if (!compact) {
    test('a connected browser\'s Work lists what the forge finds, and a found row opens in that browser', async ({ browser }) => {
      test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a connected browser');
      test.setTimeout(180_000);
      const harness = await launchHarness();
      const context = await browser.newContext();
      try {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        const { repo } = await searchProject(harness, 'work-search-connected');
        const device = await context.newPage();
        const opens = await recordOpens(device);
        const code = await redeemOnScreen(device, await mintInvite(harness, 'full'), 'Work browser');
        await confirmOnHost(harness, code);
        const { work } = await openWork(device, WEB1, false);
        await expect.poll(() => pullRows(work), { timeout: 30_000 }).toEqual(['#504 draft', '#501 open', '#502 merged', '#503 closed']);
        await work.getByTestId('ticket-work-pull').nth(2).click();
        await expect.poll(() => opens.urls).toEqual([`https://github.com/${repo}/pull/502`]);
      } finally {
        await context.close();
        await harness.close();
      }
    });

    test('a ticket of a paired computer is searched for by that computer, on its forge', async ({ page }) => {
      test.setTimeout(240_000);
      const root = await mkdtemp(join(tmpdir(), 'ao-work-search-'));
      let home: HarnessApp | undefined;
      let remote: HarnessApp | undefined;
      try {
        [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir: join(root, 'state') }));
        await pairOwnDevice(home, remote, 'Laptop');
        await searchProject(remote, 'work-search-remote');
        await home.open(page);
        const { work } = await openWork(page, WEB1, false);
        await expect.poll(() => pullRows(work), { timeout: 60_000 }).toEqual(['#504 draft', '#501 open', '#502 merged', '#503 closed']);
        expect(await forgeOps(remote, 'PullsByKey')).toHaveLength(1);
        expect(await forgeOps(home, 'PullsByKey')).toEqual([]);
      } finally {
        await remote?.close();
        await home?.close();
        await rm(root, { recursive: true, force: true });
      }
    });
  }

  test('a pull request that names a ticket carries its chip, which opens the ticket, whose Work lists it and the linked thread', async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchPullsHarness();
    try {
      await seedFakeJira(harness);
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{
          name: compact ? 'compact-ticket-work' : 'ticket-work',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# Widgets\n' } }] },
          threads: [{ title: 'Linked thread', turns: [{ userText: 'Linked thread', items: [{ kind: 'assistant_text', summary: 'done' }] }] }],
        }],
      });
      const { projectId, path, threadIds: [linked] } = seed.projects[0];
      const board = await connectFakeJira(harness, projectId);
      // Linked first: the link puts WEB-1 in this computer's copy of the
      // tracker, which is what makes WEB a connected project for the chip.
      await harness.rpc('SetThreadTicket', linked, { kind: 'jira', site: board.site, issueId: WEB1 });
      const repo: ForgeRepo = {
        forge: 'github',
        project: `ao-e2e/work-${compact ? 'compact' : 'desktop'}-${Date.now().toString(36)}`,
        pulls: [{ number: 301, title: 'WEB-1 Retry the cursor read', author: 'alice', authorName: 'Alice Ng', updatedAt: '2026-10-19T10:00:00Z' }],
      };
      await publishPullRequest(harness, path, repo, { 'src/cursor.ts': 'export const cursor = 1;\n' });
      await harness.open(page);

      const pulls = await openPage(page);
      await pickFilter(page, pulls, 'all');
      const chip = pullRow(page, 301).getByTestId('pull-ticket');
      await expect(chip).toHaveText('WEB-1', { timeout: 30_000 });
      await expect(chip).toHaveAttribute('title', 'WEB-1 First story');
      await press(chip, compact);

      const issues = page.getByTestId('issues-pane');
      const ticket = issues.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', WEB1, { timeout: 30_000 });
      await expect(issues.getByTestId('ticket-key')).toHaveText('WEB-1');
      const work = await workOf(ticket, compact);
      await expect(work.getByTestId('ticket-work-thread')).toHaveCount(1);
      await expect(work.getByTestId('ticket-work-thread')).toHaveAttribute('data-thread', linked);
      await expect(work.getByTestId('ticket-work-thread')).toContainText('Linked thread');
      const pr = work.getByTestId('ticket-work-pull');
      await expect(pr).toHaveCount(1, { timeout: 15_000 });
      await expect(pr).toContainText('#301');
      await expect(pr).toContainText('WEB-1 Retry the cursor read');
      await expect(pr.getByTestId('ticket-work-pull-state')).toHaveText('Open');
      await expect(pr).toContainText('Alice');

      // The thread's row opens it.
      await press(work.getByTestId('ticket-work-thread'), compact);
      await expectThreadShown(page, 'Linked thread', compact);
      await expect(errorToasts(page)).toHaveCount(0);
    } finally {
      await harness.close();
    }
  });

  test('Ask opens a thread linked to the ticket with its file on the unsent draft, and writes nothing to the tracker', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-ask' : 'ticket-ask');
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    await expect(work.getByTestId('ticket-work-thread')).toHaveCount(0);
    await press(work.getByTestId('ticket-work-ask'), compact);

    await expectThreadShown(page, 'WEB-1 First story', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-1.md');
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    const rows = await threadRows(harness);
    expect(rows.map((r) => [r.title, r.ticketLink?.key, r.ticketLink?.origin])).toEqual([['WEB-1 First story', 'WEB-1', 'ask']]);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);
    expect(await trackerRequests(harness, EDIT_ROUTE)).toEqual([]);
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('Ask makes its thread in the project whose board the ticket was opened from, of two that list the board', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await seedFakeJira(harness);
    const surface = compact ? 'compact' : 'desktop';
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `alpha-${surface}`, repo: {} }, { name: `beta-${surface}`, repo: {} }] });
    const [alpha, beta] = seed.projects.map((p) => p.projectId);
    const { connectionId } = await connectFakeJira(harness, alpha);
    await harness.rpc('SetProjectTrackerBoards', beta, [], [{ connection: connectionId, board: BOARD_ID, opens_first: true, name: 'WEB board', tracker_project: 'WEB' }]);
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    // The picker lists the board under each project, by project name:
    // beta's is the second.
    await press(pane.getByTestId('issues-board-picker'), compact);
    const choices = page.getByTestId('issues-board-choice');
    await expect(choices).toHaveCount(2);
    await press(choices.nth(1), compact);
    await expect(card(pane, WEB1)).toBeVisible({ timeout: 30_000 });
    await press(card(pane, WEB1).getByTestId('issues-card-open'), compact);
    const ticket = pane.getByTestId('ticket-screen');
    await expect(ticket).toHaveAttribute('data-issue', WEB1);
    const work = await workOf(ticket, compact);
    await expect(work.getByTestId('ticket-work-project')).toHaveValue(beta);
    await press(work.getByTestId('ticket-work-ask'), compact);

    await expectThreadShown(page, 'WEB-1 First story', compact);
    const rows = await threadRows(harness);
    expect(rows.map((r) => [r.projectId, r.ticketLink?.origin])).toEqual([[beta, 'ask']]);
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('Start work moves and assigns the ticket once, makes its thread on the branch named after it, saves the defaults and opens it', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-start-work' : 'start-work');
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible();
    await expect(form.getByTestId('start-work-branch')).toHaveValue('WEB-2-second-task');
    await expect(form.getByTestId('start-work-status')).toHaveValue('');
    await expect(form.getByTestId('start-work-assign')).toBeChecked();
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-save'), compact);
    await press(form.getByTestId('start-work-go'), compact);

    await expectThreadShown(page, 'WEB-2 Second task', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-2.md');
    await expect(page.getByLabel('Message Input')).toHaveValue('Work on WEB-2: Second task.');
    const [made] = await threadRows(harness);
    expect(made).toMatchObject({ title: 'WEB-2 Second task', ticketLink: { key: 'WEB-2', origin: 'start_work' } });
    expect(harnessGit(harness, made.worktreePath!, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('WEB-2-second-task');
    const moves = await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', WEB2]);
    expect(moves.map((r) => r.status)).toEqual([204]);
    const edits = await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', WEB2]);
    expect(edits.map((r) => r.body)).toEqual([{ fields: { assignee: { accountId: 'acc-user1' } } }]);
    const setup = await harness.rpc<{ setup: { start_work: unknown[] } }>('ProjectTrackerSetup', board.projectId);
    expect(setup.setup.start_work).toEqual([{ tracker: 'jira', site: board.site, project: 'WEB', status: '10001', assign: true, branch: '{key}-{title}' }]);

    // The ticket shows both changes, and the next Start work starts from
    // the saved defaults and says a thread from Start work exists.
    if (compact) {
      await page.getByTestId('chat-header').getByTestId('compact-back').click();
      await openIssuesPane(page, compact);
    }
    const status = ticket.locator('[data-testid="ticket-narrow-row"], [data-testid="ticket-rail"]').first().locator('[data-testid="ticket-field"][data-field="status"]');
    await expect(status).toContainText('In Progress', { timeout: 15_000 });
    const again = await workOf(ticket, compact);
    await expect(again.getByTestId('ticket-work-thread')).toHaveAttribute('data-thread', made.id);
    await press(again.getByTestId('ticket-work-start'), compact);
    await expect(form.getByTestId('start-work-started')).toContainText('WEB-2 already has a thread from Start work: WEB-2 Second task');
    await expect(form.getByTestId('start-work-status-already')).toHaveText('Already in In Progress.');
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('a move the tracker refuses leaves the thread made, and Try again sends only the move and opens that thread', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-start-work-refused' : 'start-work-refused');
    await harness.rpc('HarnessTrackerFault', {
      origin: { tracker: 'jira', site: JIRA_SITE }, route: MOVE_ROUTE, kind: 'refuse', times: 1, status: 400,
      body: { errorMessages: ['A resolution is required to move this issue.'], errors: {} },
    });
    await harness.open(page);
    const { work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-go'), compact);

    const run = work.getByTestId('start-work-run');
    const step = (name: string) => run.locator(`[data-testid="start-work-step"][data-step="${name}"]`);
    await expect(step('move')).toHaveAttribute('data-outcome', 'refused', { timeout: 30_000 });
    await expect(step('move')).toContainText('A resolution is required to move this issue.');
    await expect(step('thread')).toHaveAttribute('data-outcome', 'applied');
    await expect(step('assign')).toHaveAttribute('data-outcome', 'applied');
    await expect(run).not.toHaveAttribute('data-running', '');
    const [made] = await threadRows(harness);
    expect(made).toMatchObject({ title: 'WEB-2 Second task', ticketLink: { key: 'WEB-2', origin: 'start_work' } });
    // The thread is made and listed, and not opened over the refusal.
    await expect(page.getByTestId('issues-pane').getByTestId('ticket-screen')).toBeVisible();

    await press(run.getByTestId('start-work-retry'), compact);
    await expectThreadShown(page, 'WEB-2 Second task', compact);
    expect((await threadRows(harness)).map((r) => r.id)).toEqual([made.id]);
    const moves = await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', WEB2]);
    expect(moves.map((r) => r.status)).toEqual([400, 204]);
    expect(await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', WEB2])).toHaveLength(1);
  });

  test('a move from the Status field after Start work\'s refused move shows under the field, never under the Start work row', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-start-work-own-write' : 'start-work-own-write');
    const origin = { tracker: 'jira', site: JIRA_SITE };
    // The run's move is refused; the Status field's move is held.
    await harness.rpc('HarnessTrackerFault', {
      origin, route: MOVE_ROUTE, kind: 'refuse', times: 1, status: 400,
      body: { errorMessages: ['A resolution is required to move this issue.'], errors: {} },
    });
    await harness.rpc('HarnessTrackerFault', { origin, route: MOVE_ROUTE, kind: 'hold', times: 1 });
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-go'), compact);
    const move = step(work, 'move');
    await expect(move).toHaveAttribute('data-outcome', 'refused', { timeout: 30_000 });
    // Its own refusal, while it is the status's latest write.
    await expect(move.getByTestId('ticket-write-outcome')).toContainText('A resolution is required to move this issue.');

    // The Status field: the rail's, or the narrow row's under Details.
    if (compact) await press(ticket.getByTestId('ticket-details-close'), compact);
    const fields = compact ? ticket.getByTestId('ticket-narrow-row') : ticket.getByTestId('ticket-rail');
    await press(fields.locator('[data-testid="ticket-field"][data-field="status"]').getByTestId('ticket-field-value'), compact);
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'Done' }), compact);
    await expect(fields.locator('[data-testid="ticket-write-pending"][data-target="status"]')).toHaveText('Saving…', { timeout: 15_000 });
    const again = await workOf(ticket, compact);
    const row = step(again, 'move');
    await expect(row).toContainText('Move the ticket to In Progress');
    await expect(row).toContainText('Refused.');
    await expect(row.getByTestId('ticket-write-pending')).toHaveCount(0);
    await expect(row.getByTestId('ticket-write-outcome')).toHaveCount(0);
    await expect(row).not.toContainText('A resolution is required');

    await harness.rpc('HarnessTrackerRelease', { origin, route: MOVE_ROUTE });
    await expect.poll(async () => (await trackerRequests(harness, MOVE_ROUTE, ['WEB-2', WEB2])).map((r) => r.status), { timeout: 15_000 }).toEqual([400, 204]);
    await expect(ticket.locator('[data-testid="ticket-field"][data-field="status"]').first()).toContainText('Done', { timeout: 15_000 });
    await expect(row.getByTestId('ticket-write-outcome')).toHaveCount(0);
    await expect(row).toContainText('Refused.');
  });

  test('while the run\'s move is out its row says Saving… and the assignment waits, and the thread\'s row names its project', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const name = compact ? 'compact-start-work-saving' : 'start-work-saving';
    await connectFakeJiraBoard(harness, name);
    const origin = { tracker: 'jira', site: JIRA_SITE };
    await harness.rpc('HarnessTrackerFault', { origin, route: MOVE_ROUTE, kind: 'hold', times: 1 });
    await harness.open(page);
    const { work } = await openWork(page, WEB2, compact, 'To Do');
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await expect(form.getByTestId('start-work-assign')).toBeChecked();
    await press(form.getByTestId('start-work-go'), compact);

    await expect(step(work, 'move')).toContainText('Saving…', { timeout: 30_000 });
    await expect(step(work, 'assign')).toContainText('Waiting.');
    await expect(step(work, 'thread')).toContainText(`Make the thread in ${name} on WEB-2-second-task`);
    await harness.rpc('HarnessTrackerRelease', { origin, route: MOVE_ROUTE });
    await expectThreadShown(page, 'WEB-2 Second task', compact);
    expect((await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', WEB2])).map((r) => r.body)).toEqual([{ fields: { assignee: { accountId: 'acc-user1' } } }]);
  });

  test('a card\'s Ask makes its thread in the board\'s project, with nothing on the tracker', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-card-ask' : 'card-ask');
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    await showColumn(pane, 'To Do', compact);
    // Work's store loads when Ask is chosen, not when the menu opens.
    const workChunk: string[] = [];
    page.on('request', (r) => { if (/\/ticketWork\.svelte-[^/]*\.js$/.test(r.url())) workChunk.push(r.url()); });
    const menu = await cardMenuMore(page, pane, WEB2, 'WEB-2', compact);
    expect(workChunk).toEqual([]);
    await press(menu.getByTestId('issues-card-menu-ask'), compact);
    await expect.poll(() => workChunk.length).toBe(1);

    await expectThreadShown(page, 'WEB-2 Second task', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-2.md');
    const rows = await threadRows(harness);
    expect(rows.map((r) => [r.projectId, r.ticketLink?.key, r.ticketLink?.origin])).toEqual([[board.projectId, 'WEB-2', 'ask']]);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);
    expect(await trackerRequests(harness, EDIT_ROUTE)).toEqual([]);
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test('a Backlog row\'s Ask makes its thread in the board\'s project, with nothing on the tracker', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-backlog-row-ask' : 'backlog-row-ask');
    const menu = await backlogRowMenuMore(harness, page, compact);
    await press(menu.getByTestId('issues-card-menu-ask'), compact);
    await expectThreadShown(page, 'WEB-3 Backlog task', compact);
    await expect(page.getByTestId('attachment-file-chip')).toContainText('WEB-3.md');
    const rows = await threadRows(harness);
    expect(rows.map((r) => [r.projectId, r.ticketLink?.key, r.ticketLink?.origin])).toEqual([[board.projectId, 'WEB-3', 'ask']]);
    expect(await trackerRequests(harness, MOVE_ROUTE)).toEqual([]);
    expect(await trackerRequests(harness, EDIT_ROUTE)).toEqual([]);
  });

  test('a Backlog row\'s Start work… opens its ticket with the Start work form', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-backlog-row-start' : 'backlog-row-start');
    const menu = await backlogRowMenuMore(harness, page, compact);
    await press(menu.getByTestId('issues-card-menu-start-work'), compact);
    await expect(page.getByTestId('issues-pane').getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10103', { timeout: 30_000 });
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible({ timeout: 30_000 });
    await expect(form.getByTestId('start-work-branch')).toHaveValue('WEB-3-backlog-task');
  });

  test('a card\'s Start work… opens its ticket with the Start work form, and the run\'s rows show in the ticket\'s Work', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const name = compact ? 'compact-card-start-work' : 'card-start-work';
    await connectFakeJiraBoard(harness, name);
    const origin = { tracker: 'jira', site: JIRA_SITE };
    // The move is refused, so the run stays on the ticket.
    await harness.rpc('HarnessTrackerFault', {
      origin, route: MOVE_ROUTE, kind: 'refuse', times: 1, status: 400,
      body: { errorMessages: ['A resolution is required to move this issue.'], errors: {} },
    });
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    await showColumn(pane, 'To Do', compact);
    const menu = await cardMenuMore(page, pane, WEB2, 'WEB-2', compact);
    await press(menu.getByTestId('issues-card-menu-start-work'), compact);

    const ticket = pane.getByTestId('ticket-screen');
    await expect(ticket).toHaveAttribute('data-issue', WEB2, { timeout: 30_000 });
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible({ timeout: 30_000 });
    await expect(form.getByTestId('start-work-branch')).toHaveValue('WEB-2-second-task');
    await form.getByTestId('start-work-status').selectOption({ label: 'In Progress' });
    await press(form.getByTestId('start-work-go'), compact);

    const work = ticket.getByTestId('ticket-rail').getByTestId('ticket-work');
    await expect(step(work, 'move')).toHaveAttribute('data-outcome', 'refused', { timeout: 30_000 });
    await expect(step(work, 'thread')).toContainText(`Make the thread in ${name} on WEB-2-second-task`);
    const [made] = await threadRows(harness);
    expect(made).toMatchObject({ title: 'WEB-2 Second task', ticketLink: { key: 'WEB-2', origin: 'start_work' } });
  });

  test('a card\'s Start work… shows its form in Work when the ticket was read before', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-card-start-work-read' : 'card-start-work-read');
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    await showColumn(pane, 'To Do', compact);
    // Read once, so this computer holds the ticket when it opens again.
    await press(card(pane, WEB2).getByTestId('issues-card-open'), compact);
    const ticket = pane.getByTestId('ticket-screen');
    await expect(ticket.getByTestId('ticket-title')).toHaveText('Second task', { timeout: 30_000 });
    // The title can come from the board's card; the Description is the read's.
    await expect(ticket.locator('[data-testid="ticket-section"][data-field="description"]')).toBeVisible({ timeout: 30_000 });
    await press(pane.getByTestId(compact ? 'issues-back' : 'ticket-close'), compact);
    await expect(ticket).toHaveCount(0);

    // After a reload the ticket screen's code loads again, held until the
    // ticket's subscribe has answered, which carries that read, so the
    // screen mounts with the ticket's detail before it has measured itself.
    const subscribed = subscribeAnswered(page);
    await page.reload();
    let releaseScreen!: () => void;
    const screenHeld = new Promise<void>((resolve) => { releaseScreen = resolve; });
    await page.route(/\/TicketScreen-[^/]*\.js$/, async (route) => {
      await screenHeld;
      await route.continue();
    });
    await openIssuesPane(page, compact);
    await showColumn(pane, 'To Do', compact);
    const menu = await cardMenuMore(page, pane, WEB2, 'WEB-2', compact);
    await press(menu.getByTestId('issues-card-menu-start-work'), compact);
    const withDetail = await subscribed;
    releaseScreen();
    expect(withDetail, 'the reopened ticket\'s subscribe answered with the read its first open made').toBe(true);
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible({ timeout: 30_000 });
    await expect(form.getByTestId('start-work-branch')).toHaveValue('WEB-2-second-task');
    await expect(ticket.getByTestId('ticket-rail').getByTestId('ticket-work')).toBeVisible();
  });
}
