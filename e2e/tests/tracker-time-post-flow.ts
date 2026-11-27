// An agent's post of drafted time (docs/architecture/tracker-backend.md,
// "time_draft with post"), shared by tracker-time-post.spec.ts (desktop)
// and compact-tracker-time-post.spec.ts (compact), driven by a mock
// provider session that makes REAL MCP calls against the app's server,
// with the fake Jira site behind a verified connection. Covered: the post
// waits for the person in the composer, headed "Log time for <day>" and
// listing each entry under its ticket with its day, time and description;
// approved, the entries are logged in Jira and the thread shows the post's
// one row, "Logged 2 of 2 entries for <day>"; denied, nothing is logged and
// no row is added; and a wait that runs out (the harness's
// tracker-approval-wait) takes the prompt away, logs nothing and answers
// the agent that the person did not answer.

import { expect, type Locator, type Page } from '@playwright/test';

import { test, type SeedResult } from './fixtures.js';
import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { connectFakeJira, press, seedFakeJira, trackerRequests, twoDaysAgo } from './tracker-helpers.js';
import { TRACKER_TOOLS_SERVER, expandActivityRuns } from './tracker-agent-tools-helpers.js';

export interface TimePostSurface {
  compact: boolean;
}

const WORKLOG_ROUTE = 'POST rest/api/3/issue/{issue}/worklog';
/** A parked call's bound: above the time the person takes to answer. */
const PARKED_MS = 180_000;

/** A project on the fake site with one Claude thread titled title, its scenario set to draft day then post it. */
async function seedPoster(harness: HarnessApp, tag: string, title: string, day: string): Promise<{ path: string; threadId: string }> {
  await seedFakeJira(harness);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `tracker-time-post-${tag}`, repo: {}, threads: [{ title, provider: 'claude' }] }] });
  const project = seed.projects[0];
  await connectFakeJira(harness, project.projectId);
  await setScenario(harness, project.path, threadToolsScenario({
    name: `tracker-time-post-${tag}`, provider: 'claude', echoUserMessage: true,
    turns: [
      {
        steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'time_draft', args: { entries: [
          { day, key: 'WEB-1', hours: 1.5, reason: 'Built the login form' },
          { day, key: 'WEB-2', hours: 0.5, reason: 'Reviewed the pull request' },
        ] } } }],
        text: 'Drafted.',
      },
      { steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'time_draft', timeoutMs: PARKED_MS, args: { post: [day] } } }], text: 'Asked.' },
    ],
    afterTurns: 'silent',
  }));
  return { path: project.path, threadId: project.threadIds[0] };
}

/** The tracker tools' answer to the next time_draft call in cwd, as the model received it. */
async function answer(harness: HarnessApp, cwd: string): Promise<{ isError: boolean; text: string }> {
  const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'time_draft', cwd, timeoutMs: 60_000 });
  return { isError: Boolean(event.report.isError), text: event.report.result ?? '' };
}

/** Drafts the day, opens the thread, asks for the post, and answers its prompt once it shows. */
async function askToPost(harness: HarnessApp, page: Page, tag: string, compact: boolean): Promise<{ path: string; prompt: Locator; day: string; label: string }> {
  const { day, label } = twoDaysAgo();
  const title = `Timesheet ${tag}`;
  const { path, threadId } = await seedPoster(harness, tag, title, day);
  await harness.send(threadId, 'draft that day');
  const drafted = await answer(harness, path);
  expect(drafted.isError, drafted.text).toBe(false);
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await press(page.getByTestId('thread-row-title').filter({ hasText: title }).first(), compact);
  await harness.send(threadId, 'log it');
  const prompt = page.getByTestId('composer-pending-approval').filter({ has: page.getByTestId('tracker-write-approval') });
  await expect(prompt).toBeVisible({ timeout: 60_000 });
  await expect(prompt.getByTestId('composer-pending-approval-heading')).toHaveText(`Log time for ${label}`);
  const shown = await prompt.getByTestId('tracker-write-approval-ticket').evaluateAll((els) => els.map((el) => [
    (el.firstElementChild?.textContent ?? '').replace(/\s+/g, ' ').trim(),
    [...el.querySelectorAll('[data-testid="tracker-write-approval-change"]')].map((c) => c.textContent ?? ''),
  ]));
  expect(shown).toEqual([
    ['WEB-1 First story', [`${label}, 1h 30m: Built the login form`]],
    ['WEB-2 Second task', [`${label}, 30m: Reviewed the pull request`]],
  ]);
  if (compact) {
    const box = (await prompt.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize()!.width);
  }
  expect(await trackerRequests(harness, WORKLOG_ROUTE), 'logged before the person answered').toHaveLength(0);
  return { path, prompt, day, label };
}

function postRow(page: Page): Locator {
  return page.locator('[data-testid="notification-row"][data-notification-kind="time_post"]');
}

export function registerTimePostFlow({ compact }: TimePostSurface): void {
  test('an approved post logs the entries the prompt showed and adds its row', async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { path, prompt, day, label } = await askToPost(harness, page, compact ? 'approve-compact' : 'approve', compact);
    await press(prompt.getByTestId('approval-allow'), compact);
    const posted = await answer(harness, path);
    expect(posted.isError, posted.text).toBe(false);
    expect(JSON.parse(posted.text)).toEqual({ logged: [{ day, key: 'WEB-1', hours: 1.5 }, { day, key: 'WEB-2', hours: 0.5 }] });
    expect(await trackerRequests(harness, WORKLOG_ROUTE)).toHaveLength(2);
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
    await expandActivityRuns(page);
    await expect(postRow(page)).toHaveCount(1, { timeout: 30_000 });
    await expect(postRow(page)).toContainText(`Logged 2 of 2 entries for ${label}`);
  });

  test('a denied post logs nothing and adds no row', async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { path, prompt } = await askToPost(harness, page, compact ? 'deny-compact' : 'deny', compact);
    await press(prompt.getByTestId('approval-deny'), compact);
    expect(await answer(harness, path)).toEqual({ isError: true, text: '[tracker_write_denied] The user denied this change; nothing was sent.' });
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
    expect(await trackerRequests(harness, WORKLOG_ROUTE)).toHaveLength(0);
    await expandActivityRuns(page);
    await expect(page.locator('[data-testid="notification-row"][data-notification-kind="time_draft"]')).toHaveCount(1);
    await expect(postRow(page)).toHaveCount(0);
  });

  test('a post nobody answers ends its wait, logs nothing and says so', async ({ page }) => {
    test.setTimeout(240_000);
    const harness = await launchHarness({ env: harnessTiming({ 'tracker-approval-wait': '8s' }) });
    try {
      const { path } = await askToPost(harness, page, compact ? 'unanswered-compact' : 'unanswered', compact);
      expect(await answer(harness, path)).toEqual({ isError: true, text: '[tracker_write_unanswered] The user did not answer within 8 seconds; nothing was sent.' });
      await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0, { timeout: 30_000 });
      expect(await trackerRequests(harness, WORKLOG_ROUTE)).toHaveLength(0);
      await expandActivityRuns(page);
      await expect(postRow(page)).toHaveCount(0);
    } finally {
      await harness.close();
    }
  });
}
