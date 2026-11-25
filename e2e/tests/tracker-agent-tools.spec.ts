// The tracker tools (`ao-tracker-tools`, docs/architecture/tracker-backend.md,
// "Agent tools") driven by mock provider sessions that make REAL MCP calls
// against the app's server, on Claude and Codex, with the fake Jira site
// behind a verified connection: a search finds the ticket, a read by key
// reads it, and a read with no key from a thread linked to no ticket is
// refused naming the fix. Everything past the mock's HTTP call is production
// code: session registration on a ticket source, the capability URL, the
// tool contract and the tracker service. Each answer is asserted as the
// model received it and as its timeline row shows it on the desktop;
// compact-tracker-agent-tools.spec.ts reads the rows on the phone layout.

import { test, expect } from './fixtures.js';
import { expandRow, toolRow } from './ao-tool-rows-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { TICKET_ROWS, driveTrackerTools, openTicketReader } from './tracker-agent-tools-helpers.js';

const PROVIDERS: ProviderName[] = ['claude', 'codex'];

for (const provider of PROVIDERS) {
  test(`${provider}: an agent finds and reads a ticket, and a read with no link says to name one`, async ({ harness, page }) => {
    const run = await driveTrackerTools(harness, provider, provider);
    expect(run.find.isError, run.find.text).toBe(false);
    expect(JSON.parse(run.find.text)).toEqual({
      tickets: [{ key: 'WEB-12', title: 'Login fails on Safari', status: 'In Progress', assignee: 'User 1' }],
    });
    expect(run.read.isError, run.read.text).toBe(false);
    const read = JSON.parse(run.read.text) as Record<string, unknown>;
    expect(read).toMatchObject({
      key: 'WEB-12', title: 'Login fails on Safari', status: 'In Progress', assignee: 'User 1',
      // Bodies are the ADF converter's Markdown, which ends each with a newline.
      url: 'https://example.atlassian.net/browse/WEB-12', description: 'The page reloads and shows the form again.\n',
      comments: [{ by: 'User 1', body: 'The staging proxy drops the cookie.\n' }],
    });
    expect(read).not.toHaveProperty('more_comments');
    expect(run.unlinked).toEqual({ isError: true, text: '[tracker_no_ticket] This thread is not linked to a ticket; name one with key.' });

    await openTicketReader(harness, page);
    for (const { name, label, preview, opened } of TICKET_ROWS) {
      const row = toolRow(page, label, preview);
      await expect(row, name).toHaveCount(1);
      await expect(row.getByTestId('tool-call-card-label')).toHaveText(label);
      await expandRow(row);
      await opened(row);
    }
  });
}
