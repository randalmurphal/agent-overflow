// The ticket screen's attachments, links, planning poker, estimate and
// Log time (docs/specs/issues.md, "Ticket"), shared by the desktop and
// compact projects, through the shipped UI, the real backend and the fake
// Jira. Covered: an image the description names shows in place from bytes
// the backend read from Jira, a file the description names is a chip with
// its size and Save; the attachments list newest first; files attached by
// the picker, by a drop on the ticket and by a paste into the comment box
// (whose text stays), one line saying which file goes and its place while
// one is held, a file the tracker refuses saying so beside the next one
// attached; Save writes the file into this computer's downloads and Open
// shows an image in the lightbox; a link added through the form on the
// side its phrase reads, found by key, and a link removed at once, saying
// so while it is out; planning poker only once opened, a vote, a reveal
// from the menu, the estimate set from the result, Clear votes on the
// second press, and a poker failure shown in the section only; the header's
// estimate opening its editor; Log time from the menu logging time on the
// ticket for today; a ticket whose first read fails saying so where its
// content would be, and Refresh showing it once the tracker answers.
//
// Desktop only: a connected browser attaches a multi-megabyte file through
// its connection, downloads an attachment under its own name, and opens the
// ticket in the tracker in a new tab.
//
// Only Chromium drives the drop and the paste: the Wails webviews' delivery
// of a dropped file is not exercised here.

import { randomBytes } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import path from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { PNG_BASE64, PNG_BYTES } from './attachment-fixture.js';
import { expect, test } from './fixtures.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { recordOpens } from './preview-gateway-helpers.js';
import { JIRA_SITE, connectFakeJiraBoard, type ExtraJira } from './tracker-helpers.js';

export interface SectionsSurface {
  compact: boolean;
}

const FILES_ID = '10110';
const POKER_ID = '10111';
const JIRA = { tracker: 'jira', site: JIRA_SITE };

function paragraph(text: string) {
  return { type: 'paragraph', content: [{ type: 'text', text }] };
}

function media(id: string, alt: string) {
  return { type: 'mediaSingle', content: [{ type: 'media', attrs: { type: 'file', id, collection: '', alt } }] };
}

const hourAgo = (n: number) => new Date(Date.now() - n * 3600_000).toISOString();

// WEB-10 holds an image and a text file its description names; WEB-11 is
// estimated in points and has a round of planning poker with another
// voter's vote; WEB-1 blocks WEB-6 (a link to remove).
const SEEDED: ExtraJira = {
  projects: [],
  boards: [],
  estimation: 'customfield_10016',
  fields: [{ id: 'customfield_10016', name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } }],
  issueTypes: [{ id: '10001', name: 'Story' }, { id: '10002', name: 'Task' }].map((t) => ({
    ...t,
    screen: [{ field: 'summary', required: true }, ...['issuetype', 'description', 'assignee', 'priority', 'customfield_10016'].map((field) => ({ field }))],
  })),
  users: [{ accountId: 'acc-user2', displayName: 'User 2', email: 'user2@example.com', timeZone: 'UTC' }],
  links: [{ id: '40011', type: '10000', outward: '10101', inward: '10106' }],
  issues: [
    {
      id: FILES_ID, key: 'WEB-10', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket with files', sprints: [1],
      description: { type: 'doc', version: 1, content: [paragraph('The screen:'), media('media-shot', 'shot.png'), paragraph('The notes:'), media('media-notes', 'notes.txt')] },
      attachments: [
        { id: '70001', filename: 'notes.txt', mimeType: 'text/plain', content: Buffer.from('notes\n').toString('base64'), author: 'acc-user1', created: hourAgo(3), mediaId: 'media-notes' },
        { id: '70002', filename: 'shot.png', mimeType: 'image/png', content: PNG_BASE64, author: 'acc-user1', created: hourAgo(2), mediaId: 'media-shot' },
      ],
    },
    {
      id: POKER_ID, key: 'WEB-11', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket to estimate', sprints: [1],
      values: { customfield_10016: 3 },
    },
    { id: '10106', key: 'WEB-6', project: 'WEB', type: '10002', status: '10001', summary: 'Linked ticket', sprints: [1] },
  ],
  poker: { rounds: [{ issue: POKER_ID, votes: [{ id: '80001', account: 'acc-user2', vote: 8, created: hourAgo(1) }] }] },
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

async function openPane(page: Page, compact: boolean): Promise<Locator> {
  await page.getByTestId('sidebar-tool-issues').click();
  if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  const pane = page.getByTestId('issues-pane');
  await expect(pane).toBeVisible();
  return pane;
}

async function press(target: Locator, compact: boolean): Promise<void> {
  await (compact ? target.tap() : target.click());
}

async function openTicket(pane: Locator, issueId: string, compact: boolean): Promise<Locator> {
  await expect(card(pane, issueId)).toBeVisible({ timeout: 30_000 });
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  return ticket;
}

async function openMenu(page: Page, ticket: Locator, compact: boolean): Promise<Locator> {
  await press(ticket.getByTestId('ticket-menu-button'), compact);
  const menu = page.getByRole('menu', { name: 'Ticket actions' });
  await expect(menu).toBeVisible();
  return menu;
}

interface LoggedRequest { route?: string; path: string; status: number; held?: boolean; body?: unknown }

async function trackerRequests(harness: HarnessApp, route: string): Promise<LoggedRequest[]> {
  const log = await harness.rpc<{ requests: LoggedRequest[] }>('HarnessTrackerRequests', 0);
  return log.requests.filter((r) => r.route === route);
}

const UPLOAD = 'POST rest/api/3/issue/{issue}/attachments';

/** Drops `name` on `target` as a file dragged from the desktop. */
async function dropFile(page: Page, target: Locator, name: string, text: string): Promise<void> {
  const transfer = await page.evaluateHandle(([n, t]) => {
    const dt = new DataTransfer();
    dt.items.add(new File([t], n, { type: 'text/plain' }));
    return dt;
  }, [name, text] as const);
  await target.dispatchEvent('dragenter', { dataTransfer: transfer });
  await target.dispatchEvent('dragover', { dataTransfer: transfer });
  await target.dispatchEvent('drop', { dataTransfer: transfer });
}

/** Pastes `name` with the text `text` into `target`, as a copy from a page
 * carries both; answers whether the paste was left to the editor. */
async function pasteFile(target: Locator, name: string, text: string): Promise<boolean> {
  return target.evaluate((el, [n, t]) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', t);
    dt.items.add(new File([t], n, { type: 'text/plain' }));
    return el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  }, [name, text] as const);
}

export function registerIssuesTicketSectionsFlow({ compact }: SectionsSurface): void {
  test('a ticket shows the files its text names and its attachments, and takes files picked, dropped and pasted', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-files' : 'ticket-files', SEEDED);
    await harness.open(page);
    const pane = await openPane(page, compact);
    const ticket = await openTicket(pane, FILES_ID, compact);

    // The image the description names shows in place, from bytes read
    // from Jira; the text file is a chip with its size and Save.
    const description = ticket.locator('[data-testid="ticket-section"][data-field="description"]');
    const image = description.locator('img');
    await expect(image).toHaveCount(1, { timeout: 30_000 });
    await expect.poll(() => image.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(300);
    expect((await trackerRequests(harness, 'GET rest/api/3/attachment/content/{id}')).map((r) => r.status)).toEqual([200]);
    const notes = description.locator('[data-tracker-media]').filter({ hasText: 'notes.txt' });
    await expect(notes).toContainText('6 B');
    await expect(notes.getByRole('button', { name: 'Save' })).toBeVisible();

    // Newest first.
    const rows = ticket.getByTestId('ticket-attachment');
    await expect(rows).toHaveCount(2);
    expect(await rows.evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.name))).toEqual(['shot.png', 'notes.txt']);

    // Save writes the file into this computer's downloads; Open shows the
    // image in the lightbox.
    await press(rows.filter({ hasText: 'notes.txt' }).getByTestId('ticket-attachment-menu'), compact);
    await press(page.getByTestId('ticket-attachment-save'), compact);
    await expect(page.getByTestId('toast').filter({ hasText: /Saved/ })).toHaveCount(1, { timeout: 15_000 });
    expect(await readFile(path.join(harness.bootstrap.dataDir, 'downloads', 'notes.txt'), 'utf8')).toBe('notes\n');
    await press(rows.filter({ hasText: 'shot.png' }).getByTestId('ticket-attachment-menu'), compact);
    await press(page.getByTestId('ticket-attachment-open'), compact);
    const lightbox = page.getByRole('dialog').filter({ has: page.locator('img') });
    await expect(lightbox).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(lightbox).toHaveCount(0);

    // A picked file, held at the tracker: one line says which and its place.
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: UPLOAD, kind: 'hold', times: 1 });
    await ticket.getByTestId('ticket-attach-input').setInputFiles([
      { name: 'report.pdf', mimeType: 'application/pdf', buffer: Buffer.alloc(4_100_000, 1) },
      { name: 'second.txt', mimeType: 'text/plain', buffer: Buffer.from('second') },
    ]);
    await expect(ticket.getByTestId('ticket-uploading')).toContainText('Uploading report.pdf (4.1 MB)… 1 of 2');
    await expect.poll(async () => (await trackerRequests(harness, UPLOAD)).some((r) => r.held)).toBe(true);
    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: UPLOAD });
    await expect(rows).toHaveCount(4, { timeout: 30_000 });
    await expect(ticket.getByTestId('ticket-uploading')).toHaveCount(0);

    // A file the tracker refuses says so; the next file still goes.
    await harness.rpc('HarnessTrackerFault', {
      origin: JIRA, route: UPLOAD, kind: 'refuse', times: 1, status: 400, body: { errorMessages: ['The file type is not allowed here'], errors: {} },
    });
    await ticket.getByTestId('ticket-attach-input').setInputFiles([
      { name: 'refused.exe', mimeType: 'application/octet-stream', buffer: Buffer.from('MZ') },
      { name: 'after.txt', mimeType: 'text/plain', buffer: Buffer.from('after') },
    ]);
    const refused = ticket.locator('[data-testid="ticket-attach-outcome"][data-name="refused.exe"]');
    await expect(refused).toContainText('The file type is not allowed here', { timeout: 30_000 });
    await expect(rows.filter({ hasText: 'after.txt' })).toHaveCount(1, { timeout: 30_000 });

    // A drop anywhere on the ticket, and a paste into the comment box whose
    // text stays the box's.
    await dropFile(page, ticket.getByTestId('ticket-content'), 'dropped.txt', 'dropped');
    await expect(rows.filter({ hasText: 'dropped.txt' })).toHaveCount(1, { timeout: 30_000 });
    const box = ticket.getByTestId('ticket-comment-input');
    await box.focus();
    expect(await pasteFile(box, 'pasted.txt', 'pasted')).toBe(true);
    await expect(rows.filter({ hasText: 'pasted.txt' })).toHaveCount(1, { timeout: 30_000 });
    const sent = (await trackerRequests(harness, UPLOAD)).filter((r) => r.status === 200);
    expect(sent).toHaveLength(5);
  });

  test('a link is added on the side its phrase reads and another removed at once', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-links' : 'ticket-links', SEEDED);
    await harness.open(page);
    const pane = await openPane(page, compact);
    const ticket = await openTicket(pane, '10101', compact);
    const links = ticket.getByTestId('ticket-link-row');
    await expect(links).toHaveCount(1, { timeout: 30_000 });
    await expect(links).toContainText('WEB-6');

    // Add: WEB-1 is blocked by WEB-3, found by its key.
    await press(ticket.getByTestId('ticket-link-add'), compact);
    const form = page.getByTestId('ticket-link-form');
    await form.getByTestId('ticket-link-form-relation').selectOption({ label: 'is blocked by' });
    await press(form.getByTestId('ticket-link-form-find'), compact);
    await page.getByTestId('ticket-picker-find').pressSequentially('WEB-3');
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'WEB-3' }), compact);
    await expect(form.getByTestId('ticket-link-form-other')).toContainText('WEB-3');
    await press(form.getByTestId('ticket-link-form-save'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    const groups = ticket.getByTestId('ticket-link-group');
    await expect(groups.filter({ hasText: 'is blocked by' }).getByTestId('ticket-link')).toContainText('WEB-3', { timeout: 15_000 });
    const [added] = await trackerRequests(harness, 'POST rest/api/3/issueLink');
    expect(added.status).toBe(201);
    expect(added.body).toMatchObject({ type: { id: '10000' }, outwardIssue: { id: '10103' }, inwardIssue: { id: '10101' } });

    // Remove: sent at once, the row saying so while the tracker holds it.
    const route = 'DELETE rest/api/3/issueLink/{id}';
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route, kind: 'hold', times: 1 });
    const toWeb6 = links.filter({ hasText: 'WEB-6' });
    await press(toWeb6.getByTestId('ticket-link-remove'), compact);
    await expect(toWeb6.getByTestId('ticket-link-removing')).toHaveText('Removing…');
    await expect(toWeb6.getByTestId('ticket-link-remove')).toBeDisabled();
    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route });
    await expect(toWeb6).toHaveCount(0, { timeout: 15_000 });
    expect((await trackerRequests(harness, route)).map((r) => r.status)).toEqual([204]);
  });

  test('planning poker follows only once opened, takes a vote, a reveal, the estimate and a clear, and keeps a failure to itself', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-poker' : 'ticket-poker', { ...SEEDED, links: [] });
    await harness.open(page);
    const pane = await openPane(page, compact);
    const ticket = await openTicket(pane, POKER_ID, compact);
    const fold = ticket.getByTestId('ticket-poker-fold');
    await expect(fold).toHaveAttribute('aria-expanded', 'false', { timeout: 30_000 });
    await expect(ticket.getByTestId('ticket-poker')).toHaveCount(0);

    await press(fold, compact);
    const votes = ticket.getByTestId('ticket-poker-vote');
    await expect(votes).toHaveText([/User 2\s*voted/], { timeout: 30_000 });
    await press(ticket.getByTestId('ticket-poker-card').filter({ hasText: /^5$/ }), compact);
    await expect(votes.filter({ hasText: 'User 1' })).toContainText('5', { timeout: 15_000 });

    // Reveal from the menu, then set the estimate from the result.
    await press((await openMenu(page, ticket, compact)).getByTestId('ticket-menu-poker-reveal'), compact);
    await expect(ticket.getByTestId('ticket-poker-state')).toContainText('Revealed.', { timeout: 15_000 });
    const offers = ticket.getByTestId('ticket-poker-set-estimate');
    await expect(offers).toHaveText([/Set estimate to 5/, /Set estimate to 8/]);
    await press(offers.filter({ hasText: 'Set estimate to 8' }), compact);
    await expect(ticket.getByTestId('ticket-estimate')).toHaveAccessibleName('Estimate: 8', { timeout: 15_000 });
    expect((await trackerRequests(harness, 'PUT rest/agile/1.0/issue/{issue}/estimation')).map((r) => r.body)).toEqual([{ value: '8' }]);

    // Clear takes a second press.
    const clear = ticket.getByTestId('ticket-poker-clear');
    await press(clear, compact);
    await expect(clear).toHaveText('Clear everyone\'s votes');
    await press(clear, compact);
    await expect(votes).toHaveCount(0, { timeout: 15_000 });

    // A failure of the add-on shows in the section and nowhere else.
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route: 'POST gateway/api/graphql', kind: 'refuse', status: 500, body: { errors: [{ message: 'The add-on did not answer' }] } });
    await press(ticket.getByTestId('ticket-poker-card').filter({ hasText: /^3$/ }), compact);
    await expect(ticket.getByTestId('ticket-poker')).toContainText('planning poker is unavailable on this site right now', { timeout: 15_000 });
    await expect(ticket.getByTestId('ticket-failure')).toHaveCount(0);
    await expect(ticket.getByTestId('ticket-title')).toHaveText('Ticket to estimate');
  });

  test('the header\'s estimate opens its editor, and Log time logs time on the ticket for today', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-time' : 'ticket-time', { ...SEEDED, links: [] });
    await harness.open(page);
    const pane = await openPane(page, compact);
    const ticket = await openTicket(pane, POKER_ID, compact);
    const estimate = ticket.getByTestId('ticket-estimate');
    await expect(estimate).toHaveAccessibleName('Estimate: 3', { timeout: 30_000 });
    await press(estimate, compact);
    const input = ticket.locator('[data-testid="ticket-field"][data-field="points"]').getByTestId('ticket-field-input').first();
    await expect(input).toBeFocused();
    await input.press('ControlOrMeta+a');
    await input.pressSequentially('5');
    await input.press('Enter');
    await expect(estimate).toHaveAccessibleName('Estimate: 5', { timeout: 15_000 });

    await press((await openMenu(page, ticket, compact)).getByTestId('ticket-menu-log-time'), compact);
    const form = page.getByTestId('worklog-form');
    await expect(form.getByTestId('worklog-form-ticket')).toContainText('WEB-11');
    const now = new Date();
    const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    await expect(form.getByTestId('worklog-form-day')).toHaveValue(today);
    await form.getByTestId('worklog-form-time').fill('1h 30m');
    await form.getByTestId('worklog-form-description').fill('Estimated it');
    await press(form.getByTestId('worklog-form-save'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    const [logged] = await trackerRequests(harness, 'POST rest/api/3/issue/{issue}/worklog');
    expect(logged.status).toBe(201);
    expect(logged.body).toMatchObject({ timeSpentSeconds: 5400 });
  });

  test('a ticket whose first read fails says so where its content would be, and Refresh shows it once the tracker answers', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-ticket-first-read' : 'ticket-first-read', { ...SEEDED, links: [] });
    await harness.open(page);
    const pane = await openPane(page, compact);
    // The first read is refused; every read after it waits until released.
    const route = 'GET rest/api/3/issue/{issue}';
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route, kind: 'refuse', times: 1, status: 500, body: { errorMessages: ['Internal server error'], errors: {} } });
    await harness.rpc('HarnessTrackerFault', { origin: JIRA, route, kind: 'hold' });
    const ticket = await openTicket(pane, POKER_ID, compact);
    await expect(ticket.getByTestId('ticket-failure-empty')).toBeVisible({ timeout: 30_000 });
    await expect(ticket.getByTestId('ticket-comment-input')).toHaveCount(0);
    await press(ticket.getByTestId('ticket-refresh'), compact);
    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route });
    await expect(ticket.getByTestId('ticket-comment-input')).toBeVisible({ timeout: 30_000 });
    await expect(ticket.getByTestId('ticket-failure-empty')).toHaveCount(0);
  });

  if (!compact) {
    // A browser on the LAN: off this computer, so its saves are downloads
    // and a web link opens in it (window.open), not on the computer.
    test('a connected browser attaches a multi-megabyte file, downloads an attachment and opens the ticket in the tracker', async ({ browser }) => {
      test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a connected browser');
      test.setTimeout(180_000);
      const harness = await launchHarness();
      const context = await browser.newContext({ acceptDownloads: true });
      try {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        await connectFakeJiraBoard(harness, 'ticket-files-connected', { ...SEEDED, links: [] });
        const device = await context.newPage();
        const opens = await recordOpens(device);
        const code = await redeemOnScreen(device, await mintInvite(harness, 'full'), 'Ticket browser');
        await confirmOnHost(harness, code);
        const pane = await openPane(device, false);
        const ticket = await openTicket(pane, FILES_ID, false);
        const rows = ticket.getByTestId('ticket-attachment');
        await expect(rows).toHaveCount(2, { timeout: 30_000 });

        // Six megabytes through the connection, whole at the tracker.
        const big = randomBytes(6_000_000);
        await ticket.getByTestId('ticket-attach-input').setInputFiles({ name: 'big.bin', mimeType: 'application/octet-stream', buffer: big });
        await expect(rows.filter({ hasText: 'big.bin' })).toContainText('6.0 MB', { timeout: 60_000 });
        expect((await trackerRequests(harness, UPLOAD)).map((r) => r.status)).toEqual([200]);

        // Save is a download in this browser, under the attachment's name.
        const downloaded = device.waitForEvent('download');
        await rows.filter({ hasText: 'shot.png' }).getByTestId('ticket-attachment-menu').click();
        await device.getByTestId('ticket-attachment-save').click();
        const download = await downloaded;
        expect(download.suggestedFilename()).toBe('shot.png');
        expect(await readFile(await download.path())).toEqual(PNG_BYTES);

        // Open in the tracker opens the ticket's page in this browser.
        await ticket.getByTestId('ticket-open-tracker').click();
        await expect.poll(() => opens.urls).toEqual([expect.stringMatching(/\/browse\/WEB-10$/)]);
      } finally {
        await context.close();
        await harness.close();
      }
    });
  }
}
