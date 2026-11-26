// The tracker tools' writes (docs/architecture/tracker-backend.md, "Writes
// by agents"), shared by tracker-agent-writes.spec.ts (desktop) and
// compact-tracker-agent-writes.spec.ts (compact), driven by mock provider
// sessions that make real MCP calls against the app's server, with the fake
// Jira site behind a verified connection. Covered: one ticket's move and
// comment are applied at once and the call's timeline row names the ticket,
// whose key opens it in the Issues pane with the comment marked as the
// thread's and the move marked in its history as the agent's; a call naming
// three tickets waits for the person, its prompt in the composer listing
// each ticket's change and the comment, and is sent on Approve; one naming
// two is denied and nothing is sent; a read-only thread is refused, its row
// saying why; and a worker with no sidebar row that names two tickets asks
// the person, not its spawner: its spawner's Agents row says it needs
// approval, and the prompt is answered in the worker's composer, opened
// from that row; a ticket renamed while the prompt is open is sent nothing
// on Approve and said as changed, and the other ticket is sent. On the
// desktop project, a connected browser shows the prompt beside the
// backend's screen, and its answer sends the change and takes the prompt
// off both.

import { expect, type Locator, type Page } from '@playwright/test';

import { test, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { expandRow, toolRow } from './ao-tool-rows-helpers.js';
import { agentRow, openAgents } from './agents-tray-helpers.js';
import { confirmOnHost, redeemOnScreen, type PairingInvite } from './offhost-helpers.js';
import { awaitToolAnswer, setScenario, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { connectFakeJira, press, seedFakeJira, trackerRequests, type ExtraJira } from './tracker-helpers.js';
import { TRACKER_TOOLS_SERVER, expandActivityRuns } from './tracker-agent-tools-helpers.js';

export interface TrackerWritesSurface {
  compact: boolean;
}

// The seed's WEB-1 is In Progress, WEB-2 and WEB-3 To Do. Every move
// reaches To Do and In Progress; only In Progress reaches Done.
const SITE: ExtraJira = {
  projects: [],
  boards: [],
  transitions: [
    { id: '11', name: 'To Do', to: '10000' },
    { id: '21', name: 'In Progress', to: '10001' },
    { id: '31', name: 'Done', to: '10002', from: ['10001'] },
  ],
};

const COMMENT_ROUTE = 'POST rest/api/3/issue/{issue}/comment';
const MOVE_ROUTE = 'POST rest/api/3/issue/{issue}/transitions';
const EDIT_ROUTE = 'PUT rest/api/3/issue/{issue}';
/** A parked call's bound: above the time the person takes to answer. */
const PARKED_MS = 180_000;

/** The tracker tools' answer to the next call of `tool` in cwd, as the model received it. */
async function trackerAnswer(harness: HarnessApp, tool: string, cwd: string): Promise<{ isError: boolean; text: string }> {
  const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool, cwd, timeoutMs: 60_000 });
  return { isError: Boolean(event.report.isError), text: event.report.result ?? '' };
}

/** A project whose board is the fake site's, with one thread on provider per title. */
async function seedWriters(
  harness: HarnessApp,
  tag: string,
  threads: ReadonlyArray<{ title: string; provider: ProviderName; runtimeMode?: string }>,
): Promise<{ path: string; projectId: string; threadIds: string[]; connectionId: string }> {
  await seedFakeJira(harness, SITE);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `tracker-writes-${tag}`, repo: {}, threads }] });
  const project = seed.projects[0];
  const { connectionId } = await connectFakeJira(harness, project.projectId);
  return { path: project.path, projectId: project.projectId, threadIds: project.threadIds, connectionId };
}

/** Opens the page on the thread titled `title`, from the sidebar. */
async function openThread(harness: HarnessApp, page: Page, title: string, compact: boolean): Promise<void> {
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await press(page.getByTestId('thread-row-title').filter({ hasText: title }).first(), compact);
}

/** The thread's tracker write row. */
function writeRow(page: Page): Locator {
  return page.locator('[data-testid="notification-row"][data-notification-kind="tracker_write"]');
}

/** The composer's tracker write prompt, once it shows. */
async function writePrompt(page: Page): Promise<Locator> {
  const prompt = page.getByTestId('composer-pending-approval').filter({ has: page.getByTestId('tracker-write-approval') });
  await expect(prompt).toBeVisible({ timeout: 60_000 });
  return prompt;
}

/** Each ticket a prompt lists, as its key and title and its changes. */
async function promptTickets(prompt: Locator): Promise<Array<[string, string[]]>> {
  return prompt.getByTestId('tracker-write-approval-ticket').evaluateAll((els) => els.map((el) => [
    (el.firstElementChild?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    [...el.querySelectorAll('[data-testid="tracker-write-approval-change"]')].map((c) => c.textContent ?? ''),
  ] as [string, string[]]));
}

export function registerTrackerAgentWritesFlow({ compact }: TrackerWritesSurface): void {
  const providers: ProviderName[] = compact ? ['claude'] : ['claude', 'codex'];
  for (const provider of providers) {
    test(`${provider}: one ticket's move and comment are applied with a row whose key opens the ticket marked as the agent's`, async ({ harness, page }) => {
      test.setTimeout(240_000);
      const title = `Release notes ${provider}`;
      const { path, threadIds } = await seedWriters(harness, `one-${provider}`, [{ title, provider }]);
      await setScenario(harness, path, threadToolsScenario({
        name: `tracker-writes-one-${provider}`, provider, echoUserMessage: true,
        turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', args: { keys: ['web-1'], set: { Status: 'done' }, comment: 'Released in 1.4.' } } }], text: 'Done.' }],
      }));
      await harness.send(threadIds[0], 'close WEB-1');
      const answer = await trackerAnswer(harness, 'ticket_update', path);
      expect(answer.isError, answer.text).toBe(false);
      expect(JSON.parse(answer.text)).toEqual({ tickets: [{ key: 'WEB-1', applied: ['Status', 'comment'] }] });
      expect((await trackerRequests(harness, MOVE_ROUTE, ['WEB-1', '10101'])).map((r) => r.status)).toEqual([204]);

      await openThread(harness, page, title, compact);
      // The row is in the turn's activity run, with the tool row.
      await expandActivityRuns(page);
      const row = writeRow(page);
      await expect(row).toContainText('Changed WEB-1', { timeout: 30_000 });
      await expect(row.getByTestId('tracker-write-ticket')).toHaveText(/WEB-1\s*First story\s*· Status, comment/);
      const tool = toolRow(page, 'update', 'WEB-1');
      await expandRow(tool);
      await expect(tool.getByTestId('tool-call-card-ao-outcome')).toHaveText('1 ticket changed');

      // The row's key opens the ticket, whose comment and move are marked
      // as this thread's.
      await press(row.getByRole('button', { name: 'WEB-1' }), compact);
      const ticket = page.getByTestId('issues-pane').getByTestId('ticket-screen');
      await expect(ticket).toBeVisible({ timeout: 30_000 });
      const comment = ticket.getByTestId('ticket-comment').filter({ hasText: 'Released in 1.4.' });
      await expect(comment.getByTestId('ticket-comment-agent')).toHaveText(`From thread ${title}`, { timeout: 30_000 });
      await press(ticket.getByRole('radio', { name: 'History' }), compact);
      const moved = ticket.getByTestId('ticket-history-event').filter({ hasText: 'changed status' });
      await expect(moved.getByTestId('ticket-history-agent')).toHaveText(`Agent · ${title}`, { timeout: 30_000 });
    });
  }

  test('a call naming three tickets waits for the person: approved it is sent, and one naming two denied sends nothing', async ({ harness, page }) => {
    test.setTimeout(300_000);
    const title = 'Sprint planner';
    const { path, threadIds } = await seedWriters(harness, 'approve', [{ title, provider: 'claude' }]);
    await setScenario(harness, path, threadToolsScenario({
      name: 'tracker-writes-approve', provider: 'claude',
      turns: [
        { steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', timeoutMs: PARKED_MS, args: { keys: ['WEB-1', 'WEB-2', 'WEB-3'], set: { status: 'In Progress' }, comment: 'Picked up for this sprint.' } } }], text: 'Picked up.' },
        { steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', timeoutMs: PARKED_MS, args: { keys: ['WEB-2', 'WEB-3'], comment: 'Not this time.' } } }], text: 'Asked.' },
      ],
      afterTurns: 'silent',
    }));
    // A seeded thread is listed once it has a message.
    await harness.send(threadIds[0], 'pick up the three tickets');
    await openThread(harness, page, title, compact);
    let prompt = await writePrompt(page);
    expect(await promptTickets(prompt)).toEqual([
      // WEB-1 is in progress already: it gets the comment alone.
      ['WEB-1 First story', []],
      ['WEB-2 Second task', ['Status → In Progress']],
      ['WEB-3 Backlog task', ['Status → In Progress']],
    ]);
    await expect(prompt.getByTestId('tracker-write-approval-comment')).toContainText('Picked up for this sprint.');
    await expect(prompt.getByTestId('approval-allow-session')).toHaveCount(0);
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(0);
    await press(prompt.getByTestId('approval-allow'), compact);
    const approved = await trackerAnswer(harness, 'ticket_update', path);
    expect(approved.isError, approved.text).toBe(false);
    expect(JSON.parse(approved.text)).toEqual({ tickets: [
      // WEB-1 was in progress already: that step is said as already, not applied.
      { key: 'WEB-1', applied: ['comment'], already: ['status'] },
      { key: 'WEB-2', applied: ['status', 'comment'] },
      { key: 'WEB-3', applied: ['status', 'comment'] },
    ] });
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(3);
    // WEB-1 was in progress already: no move was sent for it.
    expect(await trackerRequests(harness, MOVE_ROUTE)).toHaveLength(2);
    await expect(writeRow(page)).toContainText('Changed 3 tickets');
    await expect(writeRow(page).getByTestId('tracker-write-ticket')).toHaveCount(3);

    await harness.send(threadIds[0], 'and tell two of them no');
    prompt = await writePrompt(page);
    await press(prompt.getByTestId('approval-deny'), compact);
    const denied = await trackerAnswer(harness, 'ticket_update', path);
    expect(denied).toMatchObject({ isError: true, text: '[tracker_write_denied] The user denied this change; nothing was sent.' });
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(3);
    await expect(writeRow(page)).toHaveCount(1);
  });

  test('a ticket renamed while the prompt is open is sent nothing, and the other ticket is', async ({ harness, page }) => {
    test.setTimeout(300_000);
    const title = 'Renaming planner';
    const { path, threadIds, connectionId } = await seedWriters(harness, 'renamed', [{ title, provider: 'claude' }]);
    await setScenario(harness, path, threadToolsScenario({
      name: 'tracker-writes-renamed', provider: 'claude',
      turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', timeoutMs: PARKED_MS, args: { keys: ['WEB-2', 'WEB-3'], set: { title: 'Planned title' } } } }], text: 'Renamed.' }],
      afterTurns: 'silent',
    }));
    await harness.send(threadIds[0], 'rename the two tickets');
    await openThread(harness, page, title, compact);
    const prompt = await writePrompt(page);
    expect((await promptTickets(prompt)).map(([ticket]) => ticket)).toEqual(['WEB-2 Second task', 'WEB-3 Backlog task']);
    // Someone renames WEB-2 while the person reads the prompt.
    const edit = await harness.rpc<{ parts: Array<{ state: string }> }>('EditTrackerIssue', connectionId, { issue: '10102', fields: [{ field: 'title', typed: ["A teammate's title"] }] });
    expect(edit.parts.map((p) => p.state)).toEqual(['applied']);
    await press(prompt.getByTestId('approval-allow'), compact);
    const answer = await trackerAnswer(harness, 'ticket_update', path);
    expect(answer.isError, answer.text).toBe(false);
    expect(JSON.parse(answer.text)).toEqual({ tickets: [
      { key: 'WEB-2', refused: 'WEB-2 changed on Jira while the user was deciding, so nothing was sent to it; read it again and call again for it.', not_sent: ['title'] },
      { key: 'WEB-3', applied: ['title'] },
    ] });
    // The teammate's edit is WEB-2's only one; WEB-3 got the agent's.
    expect(await trackerRequests(harness, EDIT_ROUTE, ['WEB-2', '10102'])).toHaveLength(1);
    expect(await trackerRequests(harness, EDIT_ROUTE, ['WEB-3', '10103'])).toHaveLength(1);
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
    await expandActivityRuns(page);
    await expect(writeRow(page)).toContainText('Changed WEB-3', { timeout: 30_000 });
    // The row names the ticket by the title the call read, and the change.
    await expect(writeRow(page).getByTestId('tracker-write-ticket')).toHaveText([/WEB-3\s*Backlog task\s*· title/]);
  });

  test('a read-only thread is refused and its row says why', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const title = 'Read-only reader';
    const { path, threadIds } = await seedWriters(harness, 'read-only', [{ title, provider: 'claude', runtimeMode: 'read-only' }]);
    await setScenario(harness, path, threadToolsScenario({
      name: 'tracker-writes-read-only', provider: 'claude',
      turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', args: { keys: ['WEB-1'], comment: 'Hello.' } } }], text: 'Tried.' }],
    }));
    await harness.send(threadIds[0], 'comment on WEB-1');
    const answer = await trackerAnswer(harness, 'ticket_update', path);
    expect(answer).toMatchObject({ isError: true, text: '[tracker_read_only] This thread is read-only, so it cannot change tickets; ask the user to change its access.' });
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(0);
    await openThread(harness, page, title, compact);
    await expandActivityRuns(page);
    const tool = toolRow(page, 'update', 'WEB-1');
    await expect(tool.getByTestId('row-error-code').first()).toHaveText('tracker_read_only', { timeout: 30_000 });
    await expect(writeRow(page)).toHaveCount(0);
  });

  test('a worker with no sidebar row asks the person: its spawner\'s Agents row says so, and its composer takes the answer', async ({ harness, page }) => {
    test.setTimeout(300_000);
    const lead = 'Triage lead';
    const worker = 'Ticket worker';
    const { path: leadPath, threadIds } = await seedWriters(harness, 'worker', [{ title: lead, provider: 'claude' }]);
    const workerSeed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'tracker-writes-worker-checkout', repo: {}, threads: [] }] });
    const workerProject = workerSeed.projects[0];
    await setScenario(harness, leadPath, threadToolsScenario({
      name: 'tracker-writes-lead', provider: 'claude',
      turns: [{ steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Comment on the two tickets.', title: worker, project_id: workerProject.projectId, provider: 'claude', notify: true } } }], text: 'Started.' }, { text: 'Noted.' }],
    }));
    await setScenario(harness, workerProject.path, threadToolsScenario({
      name: 'tracker-writes-worker', provider: 'claude',
      turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', timeoutMs: PARKED_MS, args: { keys: ['WEB-1', 'WEB-2'], comment: 'Checked by the worker.' } } }], text: 'Commented.' }],
    }));
    await harness.send(threadIds[0], 'have a worker comment on WEB-1 and WEB-2');
    const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadPath, timeoutMs: 60_000 });
    expect(spawn.isError, spawn.text).toBe(false);

    await harness.open(page);
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') await page.getByTestId('compact-back').click();
    await openAgents(page, lead);
    const row = agentRow(page, spawn.value!.token);
    await expect(row.getByTestId('agent-tray-row-blocked')).toContainText('Needs approval for ticket_update', { timeout: 60_000 });
    // Only the person answers a tracker write: the lead is not asked.
    await expect(row.getByTestId('agent-tray-row-blocked')).not.toContainText(`waiting on ${lead}`);
    await press(row.getByTestId('agent-tray-row-open'), compact);
    const prompt = await writePrompt(page);
    expect(await promptTickets(prompt)).toEqual([['WEB-1 First story', []], ['WEB-2 Second task', []]]);
    await expect(prompt.getByTestId('tracker-write-approval-comment')).toContainText('Checked by the worker.');
    await press(prompt.getByTestId('approval-allow'), compact);
    const answer = await trackerAnswer(harness, 'ticket_update', workerProject.path);
    expect(answer.isError, answer.text).toBe(false);
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(2);
  });

  if (compact) return;
  test('a connected browser shows the prompt and its answer is the one that counts', async ({ harness, page, browser }) => {
    test.setTimeout(300_000);
    const title = 'Browser planner';
    const { path, threadIds } = await seedWriters(harness, 'browser', [{ title, provider: 'claude' }]);
    await setScenario(harness, path, threadToolsScenario({
      name: 'tracker-writes-browser', provider: 'claude',
      turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', timeoutMs: PARKED_MS, args: { keys: ['WEB-2', 'WEB-3'], comment: 'From the browser.' } } }], text: 'Done.' }],
    }));
    await harness.send(threadIds[0], 'comment on both');
    await openThread(harness, page, title, false);
    const context = await browser.newContext();
    try {
      const device = await context.newPage();
      const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'full');
      const code = await redeemOnScreen(device, invite, 'Tracker browser');
      await confirmOnHost(harness, code);
      await device.getByText(title, { exact: true }).click({ timeout: 30_000 });
      const onDevice = await writePrompt(device);
      // The backend's own screen shows the same prompt.
      await writePrompt(page);
      expect(await promptTickets(onDevice)).toEqual([['WEB-2 Second task', []], ['WEB-3 Backlog task', []]]);
      await onDevice.getByTestId('approval-allow').click();
      const answer = await trackerAnswer(harness, 'ticket_update', path);
      expect(answer.isError, answer.text).toBe(false);
      // Answered there, the prompt leaves every screen.
      await expect(device.getByTestId('composer-pending-approval')).toHaveCount(0);
      await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
      expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(2);
    } finally {
      await context.close();
    }
  });
}
