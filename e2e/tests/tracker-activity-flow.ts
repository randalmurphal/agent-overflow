// The activity recorder end to end (docs/specs/issues.md#activity), shared
// by tracker-activity.spec.ts (desktop) and compact-tracker-activity.spec.ts
// (compact), with the fake Jira site behind a verified connection: the
// person opens a thread linked to WEB-1 in the connected browser, its
// press is the screen's input, and half a minute later an agent in another
// thread, linked to WEB-2, reads the day through time_read, a REAL MCP call
// against the app's server. The answer counts the minute on WEB-1 and in
// the day's active time (and the phone's, on compact), and names WEB-2,
// which the agent's own turn touched, without minutes. The computer's
// recording state answers that it is writing.

import { expect } from '@playwright/test';

import { test, type SeedResult } from './fixtures.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { connectFakeJira, press, seedFakeJira } from './tracker-helpers.js';
import { TRACKER_TOOLS_SERVER } from './tracker-agent-tools-helpers.js';

export interface ActivitySurface {
  compact: boolean;
}

/** Half a minute of input, which time_read rounds to the minute. */
const READ_AFTER_MS = 35_000;

/**
 * The person's half minute on the thread: the backend measures input in
 * wall time from its own clock, which no harness knob moves, and nothing
 * happens at the half minute to await.
 */
async function holdForHalfAMinuteOfWork(since: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, READ_AFTER_MS - (Date.now() - since)));
}

function today(): string {
  const at = new Date();
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

export function registerActivityFlow({ compact }: ActivitySurface): void {
  test('the minutes the person spent on a linked thread reach time_read on its ticket', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const day = today();
    await seedFakeJira(harness);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: compact ? 'tracker-activity-compact' : 'tracker-activity',
      repo: {},
      threads: [{ title: 'Login form', provider: 'claude' }, { title: 'Timesheet', provider: 'claude' }],
    }] });
    const project = seed.projects[0];
    const [worked, agent] = project.threadIds;
    const board = await connectFakeJira(harness, project.projectId);
    await harness.rpc('SetThreadTicket', worked, { kind: 'jira', site: board.site, issueId: '10101' });
    await harness.rpc('SetThreadTicket', agent, { kind: 'jira', site: board.site, issueId: '10102' });
    await setScenario(harness, project.path, threadToolsScenario({
      name: compact ? 'tracker-activity-compact' : 'tracker-activity', provider: 'claude', echoUserMessage: true,
      turns: [{ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'time_read', args: { from: day, to: day } } }], text: 'Read.' }],
      afterTurns: 'silent',
    }));

    await harness.open(page);
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
      await page.getByTestId('compact-back').click();
    }
    const opened = Date.now();
    await press(page.getByTestId('thread-row-title').filter({ hasText: 'Login form' }).first(), compact);
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await holdForHalfAMinuteOfWork(opened);

    await harness.send(agent, 'read my day');
    const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'time_read', cwd: project.path, timeoutMs: 60_000 });
    expect(Boolean(event.report.isError), event.report.result).toBe(false);
    const answer = JSON.parse(event.report.result ?? '{}') as {
      days: Array<{ day: string; active?: number; phone?: number; tickets?: Array<{ key: string; minutes?: number }> }>;
      activity_gap?: string;
    };
    const measured = answer.days.find((d) => d.day === day);
    expect(measured, JSON.stringify(answer)).toBeDefined();
    // The press is the first input: it opened the span, and the open
    // span ends at the read, half a minute later, well within the
    // window, so it counts as one minute.
    expect(measured!.active, JSON.stringify(measured)).toBe(1);
    expect(measured!.phone, JSON.stringify(measured)).toBe(compact ? 1 : undefined);
    expect(measured!.tickets).toEqual([{ key: 'WEB-1', minutes: 1 }, { key: 'WEB-2' }]);
    expect(answer.activity_gap).toBeUndefined();
    expect(await harness.rpc('GetActivityRecording')).toEqual({ since: 0 });
  });
}
