// The activity recorder end to end (docs/specs/issues.md#activity), shared
// by tracker-activity.spec.ts (desktop) and compact-tracker-activity.spec.ts
// (compact), with the fake Jira site behind a verified connection: the
// person opens a thread linked to WEB-1 in the connected browser, and its
// press is the screen's input, which the page reports on that thread. An
// agent in another thread, linked to WEB-2, then reads the day through
// time_read, a REAL MCP call against the app's server. The worked thread
// starts no turn, so WEB-1 among the day's tickets comes only from the
// screen's report; WEB-2 is there because the agent's own turn touched it.
// The computer's recording state answers that it is writing. The minutes,
// their rounding and the phone's share are the recorder's and
// trackertools' tests, on a fake clock.

import { expect, type Page } from '@playwright/test';

import { test, type SeedResult } from './fixtures.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { connectFakeJira, press, seedFakeJira } from './tracker-helpers.js';
import { TRACKER_TOOLS_SERVER } from './tracker-agent-tools-helpers.js';
import { methodIdByName } from './offhost-helpers.js';

export interface ActivitySurface {
  compact: boolean;
}

/** Watches the page's sockets for the answer to its report of thread:
 * undefined until one arrives, then its error or null. */
function watchReport(page: Page, thread: string): () => unknown {
  const method = methodIdByName('RecordActivity');
  let answer: unknown;
  page.on('websocket', (socket) => {
    const asked = new Set<string>();
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const frame = JSON.parse(payload) as { id?: string; methodId?: number; params?: unknown[] };
      if (frame.methodId === method && frame.id && frame.params?.[0] === thread) asked.add(frame.id);
    });
    socket.on('framereceived', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const frame = JSON.parse(payload) as { id?: string; error?: unknown };
      if (answer === undefined && frame.id && asked.has(frame.id)) answer = frame.error ?? null;
    });
  });
  return () => answer;
}

function today(): string {
  const at = new Date();
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

export function registerActivityFlow({ compact }: ActivitySurface): void {
  test('input on a linked thread reaches time_read on its ticket', async ({ harness, page }) => {
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

    const reported = watchReport(page, worked);
    await harness.open(page);
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
      await page.getByTestId('compact-back').click();
    }
    await press(page.getByTestId('thread-row-title').filter({ hasText: 'Login form' }).first(), compact);
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await expect.poll(reported, { message: 'the page reports the press on the thread it opened' }).not.toBeUndefined();
    expect(reported()).toBeNull();

    await harness.send(agent, 'read my day');
    const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'time_read', cwd: project.path, timeoutMs: 60_000 });
    expect(Boolean(event.report.isError), event.report.result).toBe(false);
    const answer = JSON.parse(event.report.result ?? '{}') as {
      days: Array<{ day: string; tickets?: Array<{ key: string; minutes?: number }> }>;
      activity_gap?: string;
    };
    const measured = answer.days.find((d) => d.day === day);
    expect(measured, JSON.stringify(answer)).toBeDefined();
    expect(measured!.tickets?.map((t) => t.key), JSON.stringify(measured)).toEqual(['WEB-1', 'WEB-2']);
    expect(measured!.tickets?.[1]).toEqual({ key: 'WEB-2' });
    expect(answer.activity_gap).toBeUndefined();
    expect(await harness.rpc('GetActivityRecording')).toEqual({ since: 0 });
  });
}
