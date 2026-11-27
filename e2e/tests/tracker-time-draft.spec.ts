// The time draft (docs/architecture/tracker-backend.md, "Agent tools",
// time_draft), driven by mock provider sessions that make REAL MCP calls
// against the app's server, on Claude and Codex, with the fake Jira site
// behind a verified connection: an agent drafts a day, nothing is logged
// in Jira, the thread shows the call's one row, and the thread reads the
// call's entries through ThreadTimeEntries. Approving them is the person's
// call (PostTrackerTimeEntries), driven through the bound method in
// TestAnAgentsTimeDraftPostsOnlyOnTheApproval (internal/app); the Time
// screen's drafted entries and the row's card are a later change.

import { test, expect } from './fixtures.js';
import type { SeedResult } from './fixtures.js';
import { setScenario, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { connectFakeJira, seedFakeJira, trackerRequests } from './tracker-helpers.js';
import { TRACKER_TOOLS_SERVER, expandActivityRuns } from './tracker-agent-tools-helpers.js';

interface TimeEntry { id: number; day: string; key?: string; minutes: number; reason: string; by: string; state: string; call?: string; offers?: Array<{ key: string }> }

const WORKLOG_ROUTE = 'POST rest/api/3/issue/{issue}/worklog';

/** The day two days back on this computer, YYYY-MM-DD, and how a row's title writes it. */
function twoDaysAgo(): { day: string; label: string } {
  const at = new Date(Date.now() - 2 * 24 * 3600_000);
  const pad = (n: number) => String(n).padStart(2, '0');
  const day = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
  const noon = new Date(`${day}T12:00:00Z`);
  const label = `${noon.toLocaleDateString('en-US', { weekday: 'short', timeZone: 'UTC' })} ${noon.toLocaleDateString('en-US', { month: 'short', timeZone: 'UTC' })} ${noon.getUTCDate()}`;
  return { day, label };
}

const PROVIDERS: ProviderName[] = ['claude', 'codex'];

for (const provider of PROVIDERS) {
  test(`${provider}: an agent drafts a day, the thread shows its row, and the thread reads its entries`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { day, label } = twoDaysAgo();
    const title = `Timesheet ${provider}`;
    await seedFakeJira(harness);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `tracker-time-draft-${provider}`, repo: {}, threads: [{ title, provider }] }] });
    const project = seed.projects[0];
    const threadId = project.threadIds[0];
    const { connectionId } = await connectFakeJira(harness, project.projectId);
    await setScenario(harness, project.path, threadToolsScenario({
      name: `tracker-time-draft-${provider}`, provider, echoUserMessage: true,
      turns: [{
        steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'time_draft', args: { entries: [
          { day, key: 'WEB-1', hours: 1.5, reason: 'Built the login form' },
          { day, hours: 0.5, reason: 'Standup', offer: ['WEB-2'] },
        ] } } }],
        text: 'Drafted.',
      }],
    }));
    await harness.send(threadId, 'draft my time for that day');
    const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'time_draft', cwd: project.path, timeoutMs: 60_000 });
    expect(event.report.isError, event.report.result).toBeFalsy();
    const answer = JSON.parse(event.report.result ?? '{}') as { days: Array<{ day: string; made?: Array<{ id: number; key: string }>; questions?: number[] }>; note: string };
    expect(answer.note).toBe('Nothing is logged until the user approves it.');
    expect(await trackerRequests(harness, WORKLOG_ROUTE), 'the draft logged time').toHaveLength(0);

    // The call's row names it; the week's read holds its entries too.
    const week = await harness.rpc<{ drafted?: TimeEntry[] }>('TrackerWorklogs', connectionId, 'acc-user1', day, day);
    const call = week.drafted?.[0]?.call ?? '';
    const entries = await harness.rpc<TimeEntry[]>('ThreadTimeEntries', threadId, call);
    expect(answer.days).toEqual([{ day, made: [{ id: entries[0].id, key: 'WEB-1' }], questions: [entries[1].id] }]);
    expect(entries).toEqual([
      expect.objectContaining({ day, key: 'WEB-1', minutes: 90, reason: 'Built the login form', by: 'agent', state: 'drafted', connection: connectionId }),
      expect.objectContaining({ day, minutes: 30, reason: 'Standup', by: 'agent', state: 'drafted', offers: [expect.objectContaining({ key: 'WEB-2' })] }),
    ]);
    expect(entries[0].key).toBe('WEB-1');
    expect(entries[1].key).toBeUndefined();
    expect(entries[0].call).toBe(entries[1].call);

    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: title }).first().click();
    await expandActivityRuns(page);
    const row = page.locator('[data-testid="notification-row"][data-notification-kind="time_draft"]');
    await expect(row).toHaveCount(1, { timeout: 30_000 });
    await expect(row).toContainText(`Drafted time for ${label}`);
  });
}
