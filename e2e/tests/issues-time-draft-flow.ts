// An agent's time draft on the Time screen and on its card in the thread
// (docs/specs/issues.md, "Time"), shared by the desktop and compact
// projects, through the shipped UI, the real backend, mock provider
// sessions that make real time_draft calls, and the fake Jira. Covered:
// - the draft shows dashed in By day and By ticket, its questions in the
//   Not placed row, the compact strip marks each day, and the week stays
//   live without a reload when the agent drafts again, when a card
//   changes an entry and when entries post;
// - the thread's card shows the call's entries with the same controls,
//   and Review in Time opens the Time screen at the draft's week;
// - a day approved from Time skips an entry whose ticket and day were
//   logged by hand since, and says so, while Log now still posts it; a
//   day with an open question posts its entries and leaves it open; an
//   entry Jira refuses stays, saying why in Jira's words;
// - the call's entries approved from the card, a day at a time;
// - a question answered with an offered ticket (folded into another
//   thread's entry of that ticket and day: the card it left shows no
//   stale question), with a ticket found by key, and left out; an entry
//   changed (ticket, hours, reason) and removed; Discard asks once, and
//   the card with none of its call's entries left says so;
// - an entry whose post has an unknown outcome shows the computer's
//   sentence and Remove, offers nothing that posts it, and is logged by
//   hand from the form filled from it, then removed;
// - a draft on a paired computer, read and approved from this page.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { expect, test } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { TRACKER_TOOLS_SERVER, expandActivityRuns } from './tracker-agent-tools-helpers.js';
import { JIRA_SITE, connectFakeJira, logJiraWork, openIssuesPane, press, seedFakeJira, trackerRequests } from './tracker-helpers.js';

export interface TimeDraftSurface {
  compact: boolean;
}

const WORKLOG_ADD = 'POST rest/api/3/issue/{issue}/worklog';
const JIRA = { tracker: 'jira', site: JIRA_SITE };
const SKIPPED = 'was not posted: time was logged on it that day since it was drafted; log it alone to post it';
const MAY_HAVE = "may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry";

/** A day as YYYY-MM-DD in this machine's zone, which the page shares. */
function localDay(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** The day `n` days after this week's Monday; negative is last week. */
function weekDay(n: number): string {
  const now = new Date();
  const monday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - ((now.getDay() + 6) % 7));
  return localDay(new Date(monday.getFullYear(), monday.getMonth(), monday.getDate() + n));
}

// Last week's Monday to Wednesday: whole days in the past whatever today is.
const D1 = weekDay(-7);
const D2 = weekDay(-6);
const D3 = weekDay(-5);

interface DraftEntry { day: string; key?: string; hours: number; reason: string; offer?: string[] }

interface Drafting {
  projectId: string;
  path: string;
  threadId: string;
  title: string;
}

/** A project with one Claude thread whose turns each make one time_draft call. */
async function draftingThread(harness: HarnessApp, name: string, title: string, calls: DraftEntry[][]): Promise<Drafting> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {}, threads: [{ title, provider: 'claude' }] }] });
  const project = seed.projects[0];
  await setScenario(harness, project.path, threadToolsScenario({
    name, provider: 'claude', echoUserMessage: true,
    turns: calls.map((entries) => ({ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'time_draft', args: { entries } } }], text: 'Drafted.' })),
  }));
  return { projectId: project.projectId, path: project.path, threadId: project.threadIds[0], title };
}

/** Sends the thread's next turn and waits for its time_draft answer. */
async function draft(harness: HarnessApp, thread: Drafting): Promise<void> {
  const answered = harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'time_draft', cwd: thread.path, timeoutMs: 60_000 });
  await harness.send(thread.threadId, 'draft my time');
  const event = await answered;
  expect(event.report.isError, event.report.result).toBeFalsy();
}

/** Compact: back to the list screen, from the pane shown (the thread's, or the Issues pane's). */
async function backToList(page: Page): Promise<void> {
  await page.getByTestId('compact-back').filter({ visible: true }).first().click();
  await expect(page.locator('html')).not.toHaveAttribute('data-compact-screen', 'thread');
}

/** Shows the thread titled `title`, from the sidebar, with its runs open. */
async function showThread(page: Page, title: string, compact: boolean): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') await backToList(page);
  await press(page.getByTestId('thread-row-title').filter({ hasText: title }).first(), compact);
  await expandActivityRuns(page);
}

/** The draft cards the thread shows, in timeline order. */
function cards(page: Page): Locator {
  return page.getByTestId('time-draft-card');
}

/** A card's day group. */
function cardDay(card: Locator, day: string): Locator {
  return card.locator(`[data-testid="time-draft-card-day"][data-day="${day}"]`);
}

/** The Time screen in the Issues pane, opened from the sidebar, switched
 * to Time (its tab, or the folded picker in a narrow pane) when it is not. */
async function showTime(page: Page, compact: boolean): Promise<Locator> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') await backToList(page);
  const pane = await openIssuesPane(page, compact);
  const time = pane.getByTestId('time-screen');
  if (!(await time.isVisible())) {
    const picker = pane.getByTestId('issues-tabs-picker');
    if (await picker.isVisible()) {
      await press(picker, compact);
      await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Time' }), compact);
    } else {
      await press(pane.locator('[data-testid="issues-tab"][data-screen="time"]'), compact);
    }
  }
  await expect(time).toBeVisible({ timeout: 30_000 });
  await expect(time).toBeInViewport({ ratio: 1 });
  return time;
}

/** A card's Review in Time, pressed: the Time screen it opens, once it is
 * wholly on screen. On compact the strip glides to the Issues pane, and a
 * tap inside it before it lands is one Playwright retries by scrolling the
 * target into view, which moves the strip back toward the thread. */
async function reviewInTime(page: Page, card: Locator, compact: boolean): Promise<Locator> {
  await press(card.getByTestId('time-draft-card-review'), compact);
  const time = page.getByTestId('issues-pane').getByTestId('time-screen');
  await expect(time).toBeVisible({ timeout: 30_000 });
  await expect(time).toBeInViewport({ ratio: 1 });
  return time;
}

/** The day `day` of By day; on compact, picked first from the day strip. */
async function byDay(time: Locator, day: string, compact: boolean): Promise<Locator> {
  if (compact) await press(time.locator(`[data-testid="time-day-tab"][data-day="${day}"]`), compact);
  const shown = time.locator(`[data-testid="time-day"][data-day="${day}"]`);
  await expect(shown).toBeVisible();
  return shown;
}

/** The draft entries a surface shows, as their key and hours. */
async function draftRows(scope: Locator): Promise<string[]> {
  return scope.getByTestId('time-draft-entry').evaluateAll((els) => els.map((el) => [
    el.querySelector('[data-testid="time-draft-entry-key"]')?.textContent?.trim() ?? '',
    el.querySelector('[data-testid="time-draft-entry-hours"]')?.textContent?.trim() ?? '',
  ].join(' ')));
}

/** Replaces the text of an input as a person does: select it all and type. */
async function retype(input: Locator, text: string): Promise<void> {
  await input.press('ControlOrMeta+a');
  await input.pressSequentially(text);
}

/** The worklogs the fake Jira was asked to add since `from`, by issue ID. */
async function added(harness: HarnessApp, from: number): Promise<string[]> {
  return (await trackerRequests(harness, WORKLOG_ADD)).slice(from).map((r) => r.path.split('/')[5]);
}

export function registerIssuesTimeDraftFlow({ compact }: TimeDraftSurface): void {
  test('a draft shows on Time and on its card, stays live, and a day posts what it can, saying what did not and why', async ({ harness, page }) => {
    test.setTimeout(240_000);
    const jiraURL = await seedFakeJira(harness);
    const thread = await draftingThread(harness, compact ? 'compact-time-draft' : 'time-draft', 'Timesheet', [
      [
        { day: D1, key: 'WEB-1', hours: 1.5, reason: 'Built the login form' },
        { day: D1, key: 'WEB-2', hours: 1, reason: 'Reviewed the pull request' },
        { day: D2, key: 'WEB-1', hours: 2, reason: 'Fixed the tests' },
        { day: D2, hours: 0.5, reason: 'A 30 minute call at 14:05 is not on your calendar.', offer: ['WEB-2'] },
      ],
      [{ day: D3, key: 'WEB-3', hours: 1, reason: 'Groomed the backlog' }],
    ]);
    await connectFakeJira(harness, thread.projectId);
    await draft(harness, thread);
    expect(await trackerRequests(harness, WORKLOG_ADD), 'the draft logged time').toHaveLength(0);

    // The card shows the call's entries by day.
    await harness.open(page);
    await showThread(page, thread.title, compact);
    const card = cards(page).first();
    await expect(card.getByTestId('time-draft-card-header')).toHaveText(/Log time\s*2 days\s*4h 30m/, { timeout: 30_000 });
    expect(await draftRows(cardDay(card, D1))).toEqual(['WEB-1 1h 30m', 'WEB-2 1h']);
    expect(await draftRows(cardDay(card, D2))).toEqual(['WEB-1 2h']);
    await expect(cardDay(card, D2).getByTestId('time-draft-question-text')).toHaveText('A 30 minute call at 14:05 is not on your calendar. Which ticket was it for?');

    // Review in Time opens Time at the draft's week, which shows it dashed.
    const time = await reviewInTime(page, card, compact);
    const banner = time.getByTestId('time-draft-banner');
    await expect(banner.getByTestId('time-draft-banner-title')).toHaveText('Your agent drafted 2 days', { timeout: 30_000 });
    await expect(banner.getByTestId('time-draft-banner-detail')).toHaveText('3 entries · 4h 30m · 1 question · from the thread "Timesheet"');
    const d1 = await byDay(time, D1, compact);
    expect(await draftRows(d1)).toEqual(['WEB-1 1h 30m', 'WEB-2 1h']);
    await expect(d1.getByTestId('time-draft-entry').first()).toHaveAttribute('data-state', 'drafted');
    if (compact) {
      await expect(time.locator(`[data-testid="time-day-tab"][data-day="${D1}"]`)).toHaveAttribute('data-mark', 'drafted');
      await expect(time.locator(`[data-testid="time-day-tab"][data-day="${D2}"]`)).toHaveAttribute('data-mark', 'question');
      await expect(time.locator(`[data-testid="time-day-tab"][data-day="${D3}"]`)).toHaveAttribute('data-mark', 'none');
    }
    // By ticket: the drafted time in its tickets' cells, the question in Not placed.
    await press(time.getByRole('radio', { name: 'By ticket' }), compact);
    const web1 = time.locator('[data-testid="time-ticket-row"][data-issue="10101"]');
    await expect(web1.locator(`[data-testid="time-cell"][data-day="${D1}"]`).getByTestId('time-cell-drafted')).toHaveText('1h 30m');
    await expect(web1.locator(`[data-testid="time-cell"][data-day="${D2}"]`).getByTestId('time-cell-drafted')).toHaveText('2h');
    await expect(time.getByTestId('time-not-placed-row').getByTestId('time-not-placed-cell')).toHaveAttribute('data-day', D2);
    await press(time.getByRole('radio', { name: 'By day' }), compact);

    // The agent drafts again while Time shows: no reload.
    await draft(harness, thread);
    await expect(banner.getByTestId('time-draft-banner-title')).toHaveText('Your agent drafted 3 days', { timeout: 30_000 });
    expect(await draftRows(await byDay(time, D3, compact))).toEqual(['WEB-3 1h']);

    // WEB-2 is logged by hand on D1 since the draft: the day's approval
    // posts WEB-1 and skips WEB-2, saying so.
    await logJiraWork(jiraURL, 'WEB-2', D1, 1800);
    const before = (await trackerRequests(harness, WORKLOG_ADD)).length;
    const day1 = await byDay(time, D1, compact);
    await press(day1.getByTestId('time-draft-approve-day'), compact);
    await expect(day1.getByTestId('time-draft-approve-day-write-outcome')).toContainText(`WEB-2, 1h, ${SKIPPED}`, { timeout: 30_000 });
    expect(await added(harness, before)).toEqual(['10101']);
    await expect.poll(() => draftRows(day1), { timeout: 30_000 }).toEqual(['WEB-2 1h']);
    await expect(day1.getByTestId('time-entry').filter({ hasText: 'WEB-1' })).toContainText('1h 30m');

    // Log now posts the skipped entry alone.
    await press(day1.getByTestId('time-draft-entry-hours'), compact);
    const form = page.getByTestId('time-draft-form');
    await press(form.getByTestId('time-draft-form-log'), compact);
    await expect(form).toHaveCount(0, { timeout: 30_000 });
    expect(await added(harness, before)).toEqual(['10101', '10102']);
    await expect.poll(() => draftRows(day1), { timeout: 30_000 }).toEqual([]);

    // Jira refuses D2's entry: it stays, saying why in Jira's words, and
    // the day's question stays open.
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: WORKLOG_ADD, kind: 'refuse', times: 1, status: 400,
      body: { errorMessages: ['Worklogs are closed for this period'], errors: {} },
    });
    const day2 = await byDay(time, D2, compact);
    await press(day2.getByTestId('time-draft-approve-day'), compact);
    const said = day2.getByTestId('time-draft-approve-day-write-outcome');
    await expect(said).toContainText('WEB-1, 2h, was refused: Worklogs are closed for this period', { timeout: 30_000 });
    await expect(said).toContainText('still open');
    await expect(day2.getByTestId('time-draft-entry-refusal')).toHaveText('Not posted: Worklogs are closed for this period', { timeout: 30_000 });
    await expect(day2.getByTestId('time-draft-question')).toHaveCount(1);

    // The card shows the same: D1 logged, D2's refusal, and approves D2 now.
    await showThread(page, thread.title, compact);
    await expect(cardDay(card, D1)).toContainText('logged 2h 30m', { timeout: 30_000 });
    await expect(cardDay(card, D2).getByTestId('time-draft-entry-refusal')).toHaveText('Not posted: Worklogs are closed for this period');
    const fromCard = (await trackerRequests(harness, WORKLOG_ADD)).length;
    await press(cardDay(card, D2).getByTestId('time-draft-card-approve-day'), compact);
    await expect(cardDay(card, D2).getByTestId('time-draft-card-approve-day-write-outcome')).toContainText('still open', { timeout: 30_000 });
    expect(await added(harness, fromCard)).toEqual(['10101']);
    await expect(card.getByTestId('time-draft-card-header')).toHaveText(/1 question left/, { timeout: 30_000 });
    // The second call's card is its own, approved from its footer.
    const second = cards(page).nth(1);
    await expect(second.getByTestId('time-draft-card-header')).toHaveText(/Log time\s*\S+ \S+ \d+\s*1h/);
    await press(second.getByTestId('time-draft-card-approve'), compact);
    await expect(second.getByTestId('time-draft-card-header')).toHaveText(/Logged 1h\s*1 worklog/, { timeout: 30_000 });
    await expect(second.getByTestId('time-draft-card-approve-write-outcome')).toContainText('Posted 1 of 1');
    expect(await added(harness, fromCard)).toEqual(['10101', '10103']);

    // Time caught up with the posts made from the card.
    const shownAgain = await showTime(page, compact);
    await expect(shownAgain.getByTestId('time-draft-banner')).toContainText('1 question left', { timeout: 30_000 });
  });

  test('a question is answered with one press, entries change and go, and Discard asks once and empties the card', async ({ harness, page }) => {
    test.setTimeout(240_000);
    await seedFakeJira(harness);
    const other = await draftingThread(harness, compact ? 'compact-time-standup' : 'time-standup', 'Standup notes', [
      [{ day: D1, key: 'WEB-2', hours: 1, reason: 'Reviewed in standup' }],
    ]);
    const thread = await draftingThread(harness, compact ? 'compact-time-questions' : 'time-questions', 'Timesheet', [[
      { day: D1, hours: 0.5, reason: 'A 30 minute call at 14:05 is not on your calendar.', offer: ['WEB-2'] },
      { day: D1, key: 'WEB-1', hours: 1, reason: 'Built the login form' },
      { day: D2, hours: 0.25, reason: 'Fifteen minutes in the support channel.', offer: ['WEB-1'] },
      { day: D2, hours: 0.5, reason: 'A lunch break the calendar calls a meeting.', offer: ['WEB-1'] },
    ]]);
    await connectFakeJira(harness, thread.projectId);
    await draft(harness, other);
    await draft(harness, thread);
    await harness.open(page);
    await showThread(page, thread.title, compact);
    const card = cards(page).first();
    await expect(card.getByTestId('time-draft-card-header')).toHaveText(/Log time/, { timeout: 30_000 });

    // The D1 question answered with WEB-2, which the other thread drafted
    // that day: the time folds into that entry, and this card shows no
    // question there after the event.
    const q1 = cardDay(card, D1).getByTestId('time-draft-question');
    await press(q1.getByTestId('time-draft-question-offer').filter({ hasText: 'WEB-2' }), compact);
    await expect(cardDay(card, D1).getByTestId('time-draft-question')).toHaveCount(0, { timeout: 30_000 });
    expect(await draftRows(cardDay(card, D1))).toEqual(['WEB-1 1h']);
    await showThread(page, other.title, compact);
    await expect.poll(() => draftRows(cardDay(cards(page).first(), D1)), { timeout: 30_000 }).toEqual(['WEB-2 1h 30m']);

    // Something else: a ticket found by its key.
    await showThread(page, thread.title, compact);
    const asked = cardDay(card, D2).getByTestId('time-draft-question');
    await expect(asked).toHaveCount(2);
    await press(asked.first().getByTestId('time-draft-question-other'), compact);
    await page.getByTestId('ticket-picker-find').pressSequentially('WEB-3');
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'WEB-3' }), compact);
    await expect.poll(() => draftRows(cardDay(card, D2)), { timeout: 30_000 }).toEqual(['WEB-3 15m']);
    // Leave it out.
    await press(cardDay(card, D2).getByTestId('time-draft-question-leave-out'), compact);
    await expect(cardDay(card, D2).getByTestId('time-draft-question')).toHaveCount(0, { timeout: 30_000 });

    // WEB-1's entry changed in its form: another ticket, its time and reason.
    await press(cardDay(card, D1).getByTestId('time-draft-entry-hours'), compact);
    const form = page.getByTestId('time-draft-form');
    await press(form.getByTestId('time-draft-form-find'), compact);
    await page.getByTestId('ticket-picker-find').pressSequentially('WEB-3');
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'WEB-3' }), compact);
    await expect(form.getByTestId('time-draft-form-ticket')).toContainText('WEB-3');
    await retype(form.getByTestId('time-draft-form-time'), '2h');
    await retype(form.getByTestId('time-draft-form-reason'), 'Groomed the backlog');
    await press(form.getByTestId('time-draft-form-keep'), compact);
    await expect(form).toHaveCount(0, { timeout: 30_000 });
    await expect.poll(() => draftRows(cardDay(card, D1)), { timeout: 30_000 }).toEqual(['WEB-3 2h']);
    await expect(cardDay(card, D1).getByTestId('time-draft-entry-reason')).toHaveText('Groomed the backlog');
    // Time shows the change.
    const time = await showTime(page, compact);
    await press(time.getByTestId('time-previous'), compact);
    expect(await draftRows(await byDay(time, D1, compact))).toEqual(['WEB-2 1h 30m', 'WEB-3 2h']);

    // Removed from its form.
    await showThread(page, thread.title, compact);
    await press(cardDay(card, D2).getByTestId('time-draft-entry-hours'), compact);
    await press(page.getByTestId('time-draft-form').getByTestId('time-draft-form-remove'), compact);
    await expect.poll(() => draftRows(cardDay(card, D2)), { timeout: 30_000 }).toEqual([]);
    await expect(cardDay(card, D2).getByTestId('time-draft-card-day-gone')).toBeVisible();

    // Discard asks once; the question left out is all that stays of the call.
    await press(card.getByTestId('time-draft-card-discard'), compact);
    await expect(card.getByTestId('time-draft-card-discard-confirm')).toHaveText('Remove the 1 entry of this draft? Nothing is logged.');
    await press(card.getByTestId('time-draft-card-discard-confirm-yes'), compact);
    await expect(card.getByTestId('time-draft-card-header')).toHaveText(/Nothing left to approve/, { timeout: 30_000 });
    await expect(card.getByTestId('time-draft-entry')).toHaveCount(0);
    // The other thread's card, discarded, has none of its call's entries
    // left, and says so.
    await showThread(page, other.title, compact);
    const theirs = cards(page).first();
    await press(theirs.getByTestId('time-draft-card-discard'), compact);
    await press(theirs.getByTestId('time-draft-card-discard-confirm-yes'), compact);
    await expect(theirs.getByTestId('time-draft-card-empty')).toHaveText("None of this draft's entries is left: they were removed, answered into another entry, or replaced by a later draft.", { timeout: 30_000 });
    expect(await trackerRequests(harness, WORKLOG_ADD)).toHaveLength(0);
  });

  test('an entry whose post may have landed says so, offers nothing that posts it, and is logged by hand then removed', async ({ harness, page }) => {
    test.setTimeout(180_000);
    await seedFakeJira(harness);
    const thread = await draftingThread(harness, compact ? 'compact-time-unknown' : 'time-unknown', 'Timesheet', [[
      { day: D1, key: 'WEB-1', hours: 1, reason: 'Built the login form' },
    ]]);
    await connectFakeJira(harness, thread.projectId);
    await draft(harness, thread);
    await harness.open(page);
    await showThread(page, thread.title, compact);
    const time = await reviewInTime(page, cards(page).first(), compact);
    const day = await byDay(time, D1, compact);
    await expect(day.getByTestId('time-draft-entry')).toHaveCount(1, { timeout: 30_000 });

    // The post lands in Jira and its answer is lost.
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: WORKLOG_ADD, kind: 'fail_after_apply', times: 1 });
    await press(day.getByTestId('time-draft-entry-hours'), compact);
    await press(page.getByTestId('time-draft-form').getByTestId('time-draft-form-log'), compact);
    const entry = day.getByTestId('time-draft-entry');
    await expect(entry).toHaveAttribute('data-state', 'unknown', { timeout: 30_000 });
    await expect(entry.getByTestId('time-draft-entry-unknown')).toHaveText(`This entry ${MAY_HAVE}.`);
    // Nothing that would post it: no form from the hours, no day approval.
    await expect(entry.locator('button[data-testid="time-draft-entry-hours"]')).toHaveCount(0);
    await expect(day.getByTestId('time-draft-approve-day')).toHaveCount(0);
    await expect(entry.getByTestId('time-draft-entry-remove')).toBeVisible();
    // The form stays open on a post that did not log its entry; Escape closes it.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('time-draft-form')).toHaveCount(0);
    const posts = (await trackerRequests(harness, WORKLOG_ADD)).length;
    expect(posts).toBe(1);

    // Logged by hand from the form filled from it: then the entry goes.
    await press(entry.getByTestId('time-draft-entry-log-by-hand'), compact);
    const form = page.getByTestId('worklog-form');
    await expect(form.getByTestId('worklog-form-time')).toHaveValue('1h');
    await expect(form.getByTestId('worklog-form-description')).toHaveValue('Built the login form');
    await expect(form.getByTestId('worklog-form-day')).toHaveValue(D1);
    await press(form.getByTestId('worklog-form-save'), compact);
    await expect(form).toHaveCount(0, { timeout: 30_000 });
    await expect(day.getByTestId('time-draft-entry')).toHaveCount(0, { timeout: 30_000 });
    expect(await added(harness, 0)).toEqual(['10101', '10101']);
  });

  test('a draft on a paired computer is read and approved from this page', async ({ page }) => {
    test.setTimeout(240_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-time-draft-'));
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir: join(root, 'state') }));
      await pairOwnDevice(home, remote, 'Laptop');
      await seedFakeJira(remote);
      const thread = await draftingThread(remote, compact ? 'compact-time-remote' : 'time-remote', 'Remote timesheet', [[
        { day: D1, key: 'WEB-1', hours: 1, reason: 'Built the login form' },
      ]]);
      await connectFakeJira(remote, thread.projectId);
      await draft(remote, thread);
      await home.open(page);
      await showThread(page, thread.title, compact);
      const card = cards(page).first();
      await expect(card.getByTestId('time-draft-card-header')).toHaveText(/Log time/, { timeout: 60_000 });
      const time = await reviewInTime(page, card, compact);
      const day = await byDay(time, D1, compact);
      expect(await draftRows(day)).toEqual(['WEB-1 1h']);
      await press(day.getByTestId('time-draft-approve-day'), compact);
      await expect(day.getByTestId('time-draft-approve-day-write-outcome')).toContainText('Posted 1 of 1', { timeout: 30_000 });
      expect(await added(remote, 0)).toEqual(['10101']);
    } finally {
      await remote?.close();
      await home?.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}
