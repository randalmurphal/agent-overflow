// A project thread whose mock provider calls the tracker tools
// (`ao-tracker-tools`) for real against the fake Jira site, shared by
// tracker-agent-tools.spec.ts (desktop, both providers) and
// compact-tracker-agent-tools.spec.ts (compact): a search, a read by key,
// and a read with no key from a thread linked to no ticket.

import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { setScenario, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { connectFakeJiraBoard } from './tracker-helpers.js';

export const TRACKER_TOOLS_SERVER = 'ao-tracker-tools';
export const TICKETS_TITLE = 'Ticket reader';

function adf(text: string): unknown {
  return { type: 'doc', version: 1, content: [{ type: 'paragraph', content: [{ type: 'text', text }] }] };
}

/** The tool answers the turn received, by tool and call order. */
export interface TrackerToolsRun {
  threadId: string;
  find: { isError: boolean; text: string };
  read: { isError: boolean; text: string };
  unlinked: { isError: boolean; text: string };
}

/**
 * Connects the fake Jira site, with WEB-12 beside the seed's issues, seeds
 * a project thread on provider, and drives one turn that finds, reads and
 * reads with no key.
 */
export async function driveTrackerTools(harness: HarnessApp, provider: ProviderName, tag: string): Promise<TrackerToolsRun> {
  const created = new Date(Date.now() - 3600_000).toISOString();
  await connectFakeJiraBoard(harness, `tracker-tools-board-${tag}`, {
    projects: [],
    boards: [],
    issues: [{
      id: '10112', key: 'WEB-12', project: 'WEB', type: '10002', status: '10001', summary: 'Login fails on Safari', sprints: [1], assignee: 'acc-user1',
      description: adf('The page reloads and shows the form again.'),
      comments: [{ id: '20001', author: 'acc-user1', body: adf('The staging proxy drops the cookie.'), created }],
    }],
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `tracker-tools-${tag}`, repo: {}, threads: [{ title: TICKETS_TITLE, provider }] }],
  });
  const project = seed.projects[0];
  const threadId = project.threadIds[0];
  await setScenario(
    harness,
    project.path,
    threadToolsScenario({
      name: `tracker-tools-${tag}`,
      provider,
      echoUserMessage: true,
      turns: [{
        steps: [
          { call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_find', args: { text: 'safari' } } },
          { call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_read', args: { key: 'WEB-12' } } },
          { call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_read', args: {} } },
        ],
        text: 'Read the ticket.',
      }],
    }),
  );
  await harness.send(threadId, 'find the Safari login ticket and read it');
  // Each wait consumes the answer it matched, so two waits for ticket_read
  // are its two calls, in order.
  const answer = async (tool: string) => {
    const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool, cwd: project.path, timeoutMs: 60_000 });
    return { isError: Boolean(event.report.isError), text: event.report.result ?? '' };
  };
  const find = await answer('ticket_find');
  const read = await answer('ticket_read');
  const unlinked = await answer('ticket_read');
  return { threadId, find, read, unlinked };
}

/** Opens the thread with its activity runs expanded, once its find row shows. */
export async function openTicketReader(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row-title').filter({ hasText: TICKETS_TITLE }).first().click();
  await expandActivityRuns(page);
  await expect(page.getByTestId('tool-call-card-label').filter({ hasText: /^find$/ }).first()).toBeAttached();
}

/** Expands every activity run the open thread shows, once the first shows. */
export async function expandActivityRuns(page: Page): Promise<void> {
  const runs = page.getByTestId('activity-run');
  await expect(runs.first()).toBeAttached({ timeout: 30_000 });
  for (let index = 0; index < (await runs.count()); index++) {
    const run = runs.nth(index);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  }
}

/** What each row's result says once open, by row. */
export const TICKET_ROWS: Array<{ name: string; label: string; preview: string; opened: (row: Locator) => Promise<void> }> = [
  {
    name: 'find', label: 'find', preview: '"safari"',
    opened: async (row) => {
      await expect(row.getByTestId('tool-call-card-ao-outcome')).toHaveText('1 ticket');
      await expect(row.getByTestId('tool-call-card-ao-line-title')).toHaveText('WEB-12 · Login fails on Safari · In Progress · User 1');
    },
  },
  {
    name: 'read', label: 'read', preview: 'WEB-12',
    opened: async (row) => {
      await expect(row.getByTestId('tool-call-card-ao-outcome')).toHaveText('WEB-12 Login fails on Safari · In Progress');
      await expect(row.getByTestId('tool-call-card-ao-preview')).toContainText('The page reloads and shows the form again.');
      await expect(row.getByTestId('tool-call-card-ao-line-title')).toContainText('User 1');
    },
  },
  {
    name: 'unlinked', label: 'read', preview: 'linked ticket',
    opened: (row) => expect(row.getByTestId('row-error-code').first()).toHaveText('tracker_no_ticket'),
  },
];
