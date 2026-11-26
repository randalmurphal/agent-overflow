// The Issues pane's ticket screen (docs/specs/issues.md, "Ticket"), shared
// by the desktop and compact projects, through the shipped UI, the real
// backend and the fake Jira. Covered: a card opens its ticket over the
// board (full width with Back on compact); a ticket with 30 comments
// mounts the newest five and the reveal control mounts 20 more, an image
// Jira holds showing as a chip naming the file; a status
// picked from the ticket's moves reaches Jira and the ticket shows it; a
// comment sent from the box reaches Jira and shows; a description edit
// that would lose a column layout is refused with what it would lose,
// typing more withdraws Save anyway until the changed text is refused
// again, and Save anyway sends what the editor holds; closing the ticket
// shows the board, which reads at
// once and holds the moved card; Escape (the phone's Back on compact)
// closes a ticket too. On desktop a ticket placed beside the board leaves
// the board in use, splits the pane at half within its bounds, and opens
// beside again, over the board while the pane is too narrow. A paired
// computer going off takes its open ticket's content with it, saying once
// in the pane that the computer is not connected, and the ticket reads
// again when the computer is back with the comment typed before it went.
//
// A ticket opened shows the points its own read found, 1.25, in the points
// row and in its board's estimate field alike, with the same digits, while
// the board's card keeps the board's last read.
//
// Fields in place: points typed, cleared and refused as a typo; a list of
// texts shown with no editor; a due date typed; an option past the first
// 50 found by typing, which asks the tracker, also when the text was
// typed before the first answer (held by the fake); the type changed from
// the header's mark; Open in the tracker reaching the computer's opener; a
// turned-off connection saying once that the ticket is no longer
// followed. Links grouped by relation open their ticket, and what was
// typed on the ticket before survives Back and goes with Close. A move
// whose screen needs an attachment says the app cannot set it; Resolve
// takes its resolution, two affected parts, a root cause and the
// description from its own screen, one read of the moves per picker, the
// description starting from the ticket's; a description that would lose
// its layout is refused, a change withdraws Save anyway, and Save anyway
// makes the move and closes the form. The move named Close, whose
// required fields Jira fills, is made with neither given and no None
// offered for them. An edit of a comment on the page
// before Jira's newest 50 re-reads that page, a delete there and a
// comment posted with every comment shown read the held page again from
// the moved boundary with no comment lost, and Refresh brings neither
// the old body nor the deleted comment back; unsent text outlasts a look
// at the history.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { longPress } from './touch-helpers.js';
import { JIRA_SITE, connectFakeJiraBoard, editJiraIssue, openIssuesPane as openPane, press, seedFakeJira, trackerRequests, type ExtraJira } from './tracker-helpers.js';

export interface TicketSurface {
  compact: boolean;
}

const ISSUE_ID = '10104';
const COMMENTS = 30;

function paragraph(text: string) {
  return { type: 'paragraph', content: [{ type: 'text', text }] };
}

function doc(...content: unknown[]) {
  return { type: 'doc', version: 1, content };
}

function column(text: string) {
  return { type: 'layoutColumn', attrs: { width: 50 }, content: [paragraph(text)] };
}

// WEB-4, in progress in the active sprint: a description with a column
// layout Markdown writes between markers, and 30 comments a minute apart.
const TICKET: ExtraJira = {
  projects: [],
  boards: [],
  issues: [{
    id: ISSUE_ID, key: 'WEB-4', project: 'WEB', type: '10002', status: '10001', summary: 'Ticket with comments', sprints: [1],
    description: doc(paragraph('Intro'), { type: 'layoutSection', content: [column('Left side'), column('Right side')] }),
    comments: Array.from({ length: COMMENTS }, (_, i) => ({
      id: String(30_001 + i),
      author: 'acc-user1',
      // The newest carries an image Jira holds, which no URL here fetches.
      body: i === COMMENTS - 1
        ? doc(paragraph(`Seeded comment ${i + 1}`), { type: 'mediaSingle', content: [{ type: 'media', attrs: { type: 'file', id: 'media-1', collection: '', alt: 'diagram.png' } }] })
        : doc(paragraph(`Seeded comment ${i + 1}`)),
      created: new Date(Date.now() - (COMMENTS - i) * 60_000 - 3600_000).toISOString(),
    })),
  }],
};

const day = (n: number) => new Date(Date.now() - n * 60_000 - 3600_000).toISOString();

// The fields and moves of the edits a person makes in place: points the
// board estimates in, a list of texts, a select of 60 options, a due date;
// a Resolve move whose screen takes the resolution, a multi-select, a
// long text and the description, and a move that needs an attachment.
// WEB-5 is edited in place; WEB-6, its description in columns, blocks
// WEB-1 and is blocked by WEB-2; WEB-7 has 60 comments
// (Jira's first page is the newest 50) and 30 changes in its history.
const EDITED: ExtraJira = {
  projects: [],
  boards: [],
  estimation: 'customfield_10016',
  fields: [
    { id: 'customfield_10016', name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } },
    { id: 'customfield_10070', name: 'Tags', schema: { type: 'array', items: 'string', custom: 'com.example.fake:texts', customId: 10070 } },
    {
      id: 'customfield_10080', name: 'Area', schema: { type: 'option', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:select', customId: 10080 },
      options: Array.from({ length: 60 }, (_, i) => ({ id: String(20_001 + i), value: `Area ${i + 1}` })),
    },
    {
      id: 'customfield_10090', name: 'Affected parts', schema: { type: 'array', items: 'option', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:multiselect', customId: 10090 },
      options: [{ id: '21001', value: 'Web' }, { id: '21002', value: 'API' }, { id: '21003', value: 'Docs' }],
    },
    { id: 'customfield_10095', name: 'Root cause', schema: { type: 'string', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:textarea', customId: 10095 } },
    { id: 'customfield_10097', name: 'Closed at', schema: { type: 'datetime', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:datetime', customId: 10097 } },
  ],
  // The fake's types, with the type itself on their edit screen as Jira's is.
  issueTypes: [
    { id: '10000', name: 'Epic', hierarchyLevel: 1 }, { id: '10001', name: 'Story' }, { id: '10002', name: 'Task' }, { id: '10003', name: 'Bug' },
  ].map((t) => ({
    ...t,
    screen: [
      { field: 'summary', required: true },
      ...['issuetype', 'description', 'assignee', 'reporter', 'priority', 'labels', 'duedate', 'parent', 'components'].map((field) => ({ field })),
      ...['customfield_10016', 'customfield_10070', 'customfield_10080', 'customfield_10090', 'customfield_10095', 'customfield_10020'].map((field) => ({ field })),
    ],
  })),
  resolutions: [{ id: '10000', name: 'Done' }, { id: '10001', name: 'Fixed' }],
  transitions: [
    { id: '11', name: 'To Do', to: '10000' },
    { id: '21', name: 'In Progress', to: '10001' },
    { id: '31', name: 'Done', to: '10002' },
    {
      id: '41', name: 'Resolve', to: '10002',
      screen: [{ field: 'resolution', required: true }, { field: 'customfield_10090' }, { field: 'customfield_10095' }, { field: 'description' }],
    },
    { id: '51', name: 'Archive', to: '10002', screen: [{ field: 'attachment', required: true }] },
    // Jira fills both: a date and time the app has no editor for, and one it has.
    {
      id: '61', name: 'Close', to: '10002',
      screen: [{ field: 'customfield_10097', required: true, hasDefault: true }, { field: 'resolution', required: true, hasDefault: true }],
    },
  ],
  links: [{ id: '40001', type: '10000', outward: '10106', inward: '10101' }, { id: '40002', type: '10000', outward: '10102', inward: '10106' }],
  issues: [
    {
      id: '10105', key: 'WEB-5', project: 'WEB', type: '10002', status: '10001', summary: 'Edited in place', sprints: [1], dueDate: '2026-01-02',
      values: { customfield_10016: 3, customfield_10070: ['A', 'B'] },
    },
    {
      id: '10106', key: 'WEB-6', project: 'WEB', type: '10002', status: '10001', summary: 'Linked and moved', sprints: [1],
      description: doc(paragraph('Linked intro'), { type: 'layoutSection', content: [column('Left side'), column('Right side')] }),
    },
    {
      id: '10107', key: 'WEB-7', project: 'WEB', type: '10002', status: '10001', summary: 'Long discussion', sprints: [1],
      comments: Array.from({ length: 60 }, (_, i) => ({ id: String(50_001 + i), author: 'acc-user1', body: doc(paragraph(`Seeded comment ${i + 1}`)), created: day(60 - i) })),
      changelog: Array.from({ length: 30 }, (_, i) => ({
        id: String(60_001 + i), author: 'acc-user1', created: day(120 - i),
        items: [{ fieldId: 'summary', field: 'summary', fieldtype: 'jira', fromString: `Title ${i}`, toString: `Title ${i + 1}` }],
      })),
    },
  ],
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** Opens the card's ticket and waits for its content. */
async function openTicket(pane: Locator, issueId: string, compact: boolean): Promise<Locator> {
  await press(card(pane, issueId).getByTestId('issues-card-open'), compact);
  const ticket = pane.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', issueId);
  await expect(pane.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'over');
  return ticket;
}

function field(ticket: Locator, id: string): Locator {
  return ticket.locator(`[data-testid="ticket-field"][data-field="${id}"]`);
}

/** The rail of the ticket's fields; a narrow ticket opens it from Details. */
async function rail(ticket: Locator, compact: boolean): Promise<Locator> {
  const details = ticket.getByTestId('ticket-details');
  if (await details.isVisible() && (await details.getAttribute('aria-expanded')) !== 'true') await press(details, compact);
  const shown = ticket.getByTestId('ticket-rail');
  await expect(shown).toBeVisible();
  return shown;
}

/** The status, assignee, priority and points: the narrow row, else the rail. */
function cardField(ticket: Locator, id: string): Locator {
  return field(ticket.locator('[data-testid="ticket-narrow-row"], [data-testid="ticket-rail"]').first(), id);
}

/** Opens a comment's menu: a right click, a long press on the phone. */
async function commentMenu(page: Page, comment: Locator, compact: boolean): Promise<Locator> {
  const menu = page.getByRole('menu', { name: 'Comment actions' });
  // The comment box stays in view over the comments' end: a person presses
  // a comment they scrolled clear of it.
  await comment.evaluate((el) => el.scrollIntoView({ block: 'center' }));
  if (compact) await longPress(page, comment.getByTestId('ticket-comment-body'), menu);
  else await comment.getByTestId('ticket-comment-body').click({ button: 'right' });
  await expect(menu).toBeVisible();
  return menu;
}

/** Replaces the text of the focused input as a person does: select it all and type. */
async function retype(input: Locator, text: string): Promise<void> {
  await input.press('ControlOrMeta+a');
  if (text) await input.pressSequentially(text);
  else await input.press('Backspace');
}

export function registerIssuesTicketFlow({ compact }: TicketSurface): void {
  test('a ticket opened from a card shows its comments a few at a time and writes its status, a comment and its description', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket' : 'issues-ticket', TICKET);
    await harness.open(page);
    const pane = await openPane(page, compact);
    await expect(card(pane, ISSUE_ID)).toContainText('Ticket with comments', { timeout: 30_000 });

    const ticket = await openTicket(pane, ISSUE_ID, compact);
    await expect(ticket.getByTestId('ticket-title')).toHaveText('Ticket with comments');
    await expect(ticket.getByTestId('ticket-key')).toHaveText('WEB-4');
    // The board under the ticket takes no input while covered.
    await expect(pane.getByTestId('issues-board-region')).toHaveAttribute('inert', '');
    if (compact) {
      await expect(ticket.getByTestId('ticket-close')).toHaveCount(0);
      await expect(pane.getByTestId('issues-back')).toBeVisible();
    }

    // The newest five of 30 comments are mounted; one press mounts 20 more.
    const comments = ticket.getByTestId('ticket-comment');
    await expect(comments).toHaveCount(5, { timeout: 30_000 });
    await expect(comments.last()).toContainText(`Seeded comment ${COMMENTS}`);
    // Its image is a chip naming the file: no image, no link.
    await expect(comments.last().locator('[data-tracker-media]')).toHaveText('diagram.png');
    await expect(comments.last().locator('img, a[href]')).toHaveCount(0);
    const earlier = ticket.getByTestId('ticket-comments-earlier');
    await expect(earlier).toHaveText('Show 20 earlier comments');
    await press(earlier, compact);
    await expect(comments).toHaveCount(25);
    await expect(comments.first()).toContainText('Seeded comment 6');
    await expect(earlier).toHaveText('Show 5 earlier comments');

    // The status, from the moves the ticket holds.
    const status = field(ticket, 'status');
    await expect(status.getByTestId('ticket-field-value')).toContainText('In Progress');
    await press(status.getByTestId('ticket-field-value'), compact);
    await press(page.getByTestId('ticket-picker-item').filter({ hasText: 'Done' }), compact);
    await expect(status.getByTestId('ticket-field-value')).toContainText('Done', { timeout: 15_000 });
    expect((await trackerRequests(harness, 'POST rest/api/3/issue/{issue}/transitions')).map((r) => r.status)).toEqual([204]);

    // A comment from the box.
    const box = ticket.getByTestId('ticket-comment-input');
    await box.fill('A comment from the ticket screen');
    await press(ticket.getByTestId('ticket-comment-send'), compact);
    await expect(comments.filter({ hasText: 'A comment from the ticket screen' })).toHaveCount(1, { timeout: 15_000 });
    await expect(box).toHaveValue('');
    expect((await trackerRequests(harness, 'POST rest/api/3/issue/{issue}/comment')).map((r) => r.status)).toEqual([201]);

    // A description edit that drops the layout's markers would lose the
    // layout: refused, nothing sent, until Save anyway.
    const description = ticket.locator('[data-testid="ticket-section"][data-field="description"]');
    await expect(description.getByTestId('ticket-section-text')).toContainText('Left side');
    await press(description.getByTestId('ticket-section-edit'), compact);
    const editor = description.getByTestId('ticket-section-editor');
    const markdown = await editor.inputValue();
    expect(markdown).toContain('<!-- layout -->');
    await editor.fill(markdown.split('\n').filter((line) => !line.startsWith('<!--')).join('\n').replace('Intro', 'Intro, edited'));
    await press(description.getByTestId('ticket-section-save'), compact);
    const outcome = description.getByTestId('ticket-write-outcome');
    await expect(outcome).toHaveAttribute('data-kind', 'refused', { timeout: 15_000 });
    await expect(outcome).toContainText('It would lose');
    expect(await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}')).toEqual([]);
    // Typing after the refusal withdraws Save anyway: the changed text is
    // checked again, and Save anyway then sends what the editor holds.
    await editor.press('ControlOrMeta+End');
    await editor.pressSequentially('\n\nTyped after the refusal.');
    await expect(description.getByTestId('ticket-write-save-anyway')).toHaveCount(0);
    await press(description.getByTestId('ticket-section-save'), compact);
    await expect(outcome).toHaveAttribute('data-kind', 'refused', { timeout: 15_000 });
    expect(await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}')).toEqual([]);
    await press(description.getByTestId('ticket-write-save-anyway'), compact);
    await expect(description.getByTestId('ticket-section-text')).toContainText('Intro, edited', { timeout: 15_000 });
    await expect(description.getByTestId('ticket-section-text')).toContainText('Typed after the refusal.');
    await expect(outcome).toHaveCount(0);
    expect((await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}')).map((r) => r.status)).toEqual([200]);

    // Closing the ticket shows the board again: it votes shown and reads at
    // its next poll, or at once when a poll came due while it was covered.
    await press(pane.getByTestId(compact ? 'issues-back' : 'ticket-close'), compact);
    await expect(ticket).toHaveCount(0);
    await expect(pane.getByTestId('issues-board-region')).not.toHaveAttribute('inert');
    if (compact) {
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
      await expect(pane.getByTestId('issues-column-tab').filter({ hasText: 'Done' })).toContainText('1', { timeout: 15_000 });
    } else {
      const done = pane.locator('[data-testid="issues-column"][data-column="Done"]');
      await expect(card(done, ISSUE_ID)).toBeVisible({ timeout: 15_000 });
    }

    // Escape, and the phone's Back, close the ticket too. The compact
    // board shows the moved card under Done.
    if (compact) await press(pane.getByTestId('issues-column-tab').filter({ hasText: 'Done' }), compact);
    const reopened = await openTicket(pane, ISSUE_ID, compact);
    await expect(reopened.getByTestId('ticket-title')).toHaveText('Ticket with comments');
    if (compact) await page.goBack();
    else await page.keyboard.press('Escape');
    await expect(reopened).toHaveCount(0);
    await expect(card(pane, ISSUE_ID)).toBeVisible();
    if (compact) await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  });

  if (!compact) {
    test('a ticket beside the board leaves the board in use, splits the pane at half, and is remembered', async ({ harness, page }) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width: 1600, height: 900 });
      await connectFakeJiraBoard(harness, 'issues-ticket-beside', TICKET);
      await harness.open(page);
      const pane = await openPane(page, false);
      await expect(card(pane, ISSUE_ID)).toBeVisible({ timeout: 30_000 });
      let ticket = await openTicket(pane, ISSUE_ID, false);
      const placed = pane.getByTestId('issues-ticket');
      const region = pane.getByTestId('issues-board-region');
      await ticket.getByRole('radio', { name: 'Beside' }).click();
      await expect(placed).toHaveAttribute('data-placement', 'beside');
      await expect(region).not.toHaveAttribute('inert');

      // The split starts at half; the ticket keeps at least 380.
      const widths = async () => ({ board: (await region.boundingBox())!.width, ticket: (await placed.boundingBox())!.width });
      await expect.poll(async () => { const w = await widths(); return Math.abs(w.board - w.ticket) <= 2; }).toBe(true);
      const half = (await widths()).board;
      const resizer = pane.getByTestId('issues-ticket-resize');
      await resizer.focus();
      await page.keyboard.press('ArrowRight');
      await expect.poll(async () => (await widths()).board).toBeCloseTo(half + 16, 0);
      await page.keyboard.press('End');
      await expect.poll(async () => Math.round((await widths()).ticket)).toBe(380);
      await resizer.dblclick();
      await expect.poll(async () => (await widths()).board).toBeCloseTo(half, 0);

      // The board beside it opens another card's ticket.
      await card(pane, '10101').getByTestId('issues-card-open').click();
      await expect(ticket).toHaveAttribute('data-issue', '10101');

      // Closed and opened again, it opens beside; a pane too narrow for both
      // shows it over the board until it is wide enough again.
      await ticket.getByTestId('ticket-close').click();
      await expect(ticket).toHaveCount(0);
      await card(pane, ISSUE_ID).getByTestId('issues-card-open').click();
      ticket = pane.getByTestId('ticket-screen');
      await expect(placed).toHaveAttribute('data-placement', 'beside');
      await page.setViewportSize({ width: 1000, height: 900 });
      await expect(placed).toHaveAttribute('data-placement', 'over');
      await expect(ticket.getByRole('radio', { name: 'Beside' })).toHaveCount(0);
      await page.setViewportSize({ width: 1600, height: 900 });
      await expect(placed).toHaveAttribute('data-placement', 'beside');
    });
  }

  test('a ticket opened shows the points its own read found, in the points row and its board\'s field alike, while the board keeps its last read', async ({ page }) => {
    test.setTimeout(90_000);
    // No poll runs within the test: the board holds 3 until it reads again.
    const harness = await launchHarness({ env: harnessTiming({ 'tracker-poll': '10m' }) });
    try {
      const board = await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket-points' : 'issues-ticket-points', EDITED);
      await harness.open(page);
      const pane = await openPane(page, compact);
      await expect(card(pane, '10105').getByTestId('issues-card-estimate')).toHaveText('3', { timeout: 30_000 });
      // A value with two decimals, which the points row prints with every
      // digit as the field does.
      await editJiraIssue(board, 'WEB-5', { customfield_10016: 1.25 });

      const ticket = await openTicket(pane, '10105', compact);
      await expect(cardField(ticket, 'points').getByTestId('ticket-field-value')).toHaveText('1.25', { timeout: 30_000 });
      const shown = await rail(ticket, compact);
      await press(shown.getByTestId('ticket-more-fields'), compact);
      await expect(field(shown, 'customfield_10016').getByTestId('ticket-field-value')).toHaveText('1.25');
      await expect(card(pane, '10105').getByTestId('issues-card-estimate')).toHaveText('3');
    } finally {
      await harness.close();
    }
  });

  test('a ticket\'s own fields change in place: points typed and cleared, a due date, an option past the first 50 and the type from its mark', async ({ harness, page }) => {
    test.setTimeout(120_000);
    const board = await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket-fields' : 'issues-ticket-fields', EDITED);
    await harness.open(page);
    const pane = await openPane(page, compact);
    await expect(card(pane, '10105')).toContainText('Edited in place', { timeout: 30_000 });
    const ticket = await openTicket(pane, '10105', compact);
    const points = cardField(ticket, 'points');
    await expect(points.getByTestId('ticket-field-value')).toHaveText('3', { timeout: 30_000 });

    // The narrow ticket's status, assignee, priority and points are one row.
    if (compact) {
      const row = ticket.getByTestId('ticket-narrow-row');
      const tops = await row.getByTestId('ticket-field').evaluateAll((els) => els.map((el) => Math.round(el.getBoundingClientRect().top)));
      expect(tops).toHaveLength(4);
      expect(new Set(tops).size).toBe(1);
    }

    // Points: typed, then cleared, as the board estimates them.
    const estimations = () => trackerRequests(harness, 'PUT rest/agile/1.0/issue/{issue}/estimation');
    await press(points.getByTestId('ticket-field-value'), compact);
    const pointsInput = points.getByTestId('ticket-field-input');
    await retype(pointsInput, '8');
    await pointsInput.press('Enter');
    await expect(points.getByTestId('ticket-field-value')).toHaveText('8', { timeout: 15_000 });
    await press(points.getByTestId('ticket-field-value'), compact);
    await retype(pointsInput, '');
    await pointsInput.press('Enter');
    await expect(points.getByTestId('ticket-field-value')).toHaveText(compact ? 'Points' : 'None', { timeout: 15_000 });
    expect((await estimations()).map((r) => [r.status, r.body])).toEqual([[200, { value: '8' }], [200, { value: null }]]);
    // A typo is not a number: Enter says so and sends nothing.
    await press(points.getByTestId('ticket-field-value'), compact);
    await pointsInput.pressSequentially('1e');
    await pointsInput.press('Enter');
    await expect(points.getByTestId('ticket-field-invalid')).toHaveText('Type a number, or clear the field.');
    expect(await estimations()).toHaveLength(2);
    await pointsInput.press('Escape');

    // A list of texts shows as it is and takes no editor, only its pin; a
    // due date is typed.
    const fields = await rail(ticket, compact);
    await press(fields.getByTestId('ticket-more-fields'), compact);
    const tags = field(fields, 'customfield_10070');
    await expect(tags.getByTestId('ticket-field-value')).toHaveText('A, B');
    await expect(tags.locator('button:not([data-testid="ticket-field-pin"]), input')).toHaveCount(0);
    const due = field(fields, 'duedate');
    await expect(due.getByTestId('ticket-field-value')).toHaveText('2026-01-02');
    await press(due.getByTestId('ticket-field-value'), compact);
    await due.getByTestId('ticket-field-input').fill('2026-03-04');
    await due.getByTestId('ticket-field-input').press('Enter');
    await expect(due.getByTestId('ticket-field-value')).toHaveText('2026-03-04', { timeout: 15_000 });
    const edits = () => trackerRequests(harness, 'PUT rest/api/3/issue/{issue}', ['WEB-5', '10105']);
    expect((await edits()).map((r) => r.body)).toEqual([{ fields: { duedate: '2026-03-04' } }]);

    // An option past the first 50: the first answer says more match, so
    // typing asks the tracker, once for the pause.
    const editmeta = () => trackerRequests(harness, 'GET rest/api/3/issue/{issue}/editmeta');
    const metaBefore = (await editmeta()).length;
    const area = field(fields, 'customfield_10080');
    await press(area.getByTestId('ticket-field-value'), compact);
    const items = page.getByTestId('ticket-picker-item');
    await expect(items).toHaveCount(50);
    await expect(items.filter({ hasText: 'Area 55' })).toHaveCount(0);
    await page.getByTestId('ticket-picker-find').pressSequentially('Area 55');
    await expect(items).toHaveText(['Area 55']);
    expect((await editmeta()).length - metaBefore).toBe(2);
    await press(items.filter({ hasText: 'Area 55' }), compact);
    await expect(area.getByTestId('ticket-field-value')).toHaveText('Area 55', { timeout: 15_000 });
    expect((await edits()).at(-1)?.body).toEqual({ fields: { customfield_10080: { id: '20055' } } });
    // Typed before the first answer lands: once it says more match, the
    // text in the box is searched.
    const route = 'GET rest/api/3/issue/{issue}/editmeta';
    const origin = { tracker: 'jira', site: JIRA_SITE };
    await harness.rpc('HarnessTrackerFault', { origin, route, kind: 'hold', times: 1 });
    const metaHeld = (await editmeta()).length;
    await press(area.getByTestId('ticket-field-value'), compact);
    await expect.poll(async () => (await editmeta()).some((r) => r.held)).toBe(true);
    await page.getByTestId('ticket-picker-find').pressSequentially('Area 57');
    await expect(page.getByTestId('ticket-picker-loading')).toBeVisible();
    await harness.rpc('HarnessTrackerRelease', { origin, route });
    await expect(items).toHaveText(['Area 57']);
    await expect(page.getByTestId('ticket-picker-none')).toHaveCount(0);
    expect((await editmeta()).length - metaHeld).toBe(2);
    await page.keyboard.press('Escape');
    await expect(items).toHaveCount(0);
    if (compact) await press(ticket.getByTestId('ticket-details-close'), compact);

    // The header's type mark changes the type.
    const mark = ticket.getByTestId('ticket-type-mark');
    await expect(mark).toHaveAttribute('aria-label', 'Type: Task');
    await press(mark, compact);
    await press(items.filter({ hasText: 'Bug' }), compact);
    await expect(mark).toHaveAttribute('aria-label', 'Type: Bug', { timeout: 15_000 });
    expect((await edits()).at(-1)?.body).toEqual({ fields: { issuetype: { id: '10003' } } });

    // Open in the tracker asks this computer's browser to open the ticket:
    // the harness has none, which a toast says.
    await press(ticket.getByTestId('ticket-open-tracker'), compact);
    await expect(page.getByTestId('toast').filter({ hasText: 'Failed to open browser' })).toHaveCount(1);

    // Its connection turned off: one sentence that the ticket is no longer
    // followed, none from the board it covers.
    await harness.rpc('SetTrackerConnectionEnabled', board.connectionId, false);
    await expect(ticket.getByTestId('ticket-ended')).toBeVisible({ timeout: 15_000 });
    await expect(pane.getByText(/no longer followed/)).toHaveCount(1);
  });

  test('a linked ticket opens through its link, keeps what was typed on the one before it, and a move takes its own fields', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket-moves' : 'issues-ticket-moves', EDITED);
    await harness.open(page);
    const pane = await openPane(page, compact);
    await expect(card(pane, '10106')).toContainText('Linked and moved', { timeout: 30_000 });
    let ticket = await openTicket(pane, '10106', compact);

    // Links, grouped by how they relate.
    const groups = ticket.getByTestId('ticket-link-group');
    await expect(groups.getByTestId('ticket-link-relation')).toHaveText(['blocks', 'is blocked by'], { timeout: 30_000 });
    await expect(groups.nth(0).getByTestId('ticket-link')).toContainText('WEB-1');
    await expect(groups.nth(1).getByTestId('ticket-link')).toContainText('WEB-2');

    // What is typed on this ticket survives following a link and coming back.
    const box = ticket.getByTestId('ticket-comment-input');
    await box.pressSequentially('Typed before following a link');
    const description = ticket.locator('[data-testid="ticket-section"][data-field="description"]');
    await press(description.getByTestId('ticket-section-edit'), compact);
    await retype(description.getByTestId('ticket-section-editor'), 'A description typed before the link');
    await press(groups.nth(0).getByTestId('ticket-link'), compact);
    await expect(ticket).toHaveAttribute('data-issue', '10101');
    await expect(ticket.getByTestId('ticket-title')).toHaveText('First story');
    if (!compact) await expect(ticket.getByTestId('ticket-back')).toBeVisible();
    // The menu goes back one ticket or closes them all.
    await press(ticket.getByTestId('ticket-menu-button'), compact);
    await expect(page.getByTestId('ticket-menu-back')).toBeVisible();
    await expect(page.getByTestId('ticket-menu-close')).toBeVisible();
    await press(page.getByTestId('ticket-menu-back'), compact);
    await expect(ticket).toHaveAttribute('data-issue', '10106');
    await expect(box).toHaveValue('Typed before following a link');
    await expect(description.getByTestId('ticket-section-editor')).toHaveValue('A description typed before the link');
    // Closed with the menu from the linked ticket, the trail goes and what
    // was typed with it.
    await press(groups.nth(1).getByTestId('ticket-link'), compact);
    await expect(ticket).toHaveAttribute('data-issue', '10102');
    await press(ticket.getByTestId('ticket-menu-button'), compact);
    await press(page.getByTestId('ticket-menu-close'), compact);
    await expect(ticket).toHaveCount(0);
    ticket = await openTicket(pane, '10106', compact);
    await expect(ticket.getByTestId('ticket-comments')).toBeVisible({ timeout: 30_000 });
    await expect(box).toHaveValue('');
    await expect(description.getByTestId('ticket-section-editor')).toHaveCount(0);

    // A move that needs a field the app cannot set says so, with the way
    // to the tracker and no Move.
    const status = cardField(ticket, 'status');
    const moves = page.getByTestId('ticket-picker-item');
    await press(status.getByTestId('ticket-field-value'), compact);
    await press(moves.filter({ hasText: 'Archive' }), compact);
    const form = ticket.getByTestId('ticket-move-form');
    await expect(form.getByTestId('ticket-move-unsettable')).toHaveText('Moving to Done needs Attachment, which the app cannot set. Open the ticket in the tracker to make this move.');
    await expect(form.getByTestId('ticket-move-open-tracker')).toBeVisible();
    await expect(form.getByTestId('ticket-move-submit')).toHaveCount(0);
    await press(form.getByTestId('ticket-move-cancel'), compact);
    await expect(form).toHaveCount(0);

    // Resolve takes the resolution, several affected parts and a root
    // cause, each from the move's own screen: one read of the moves per
    // picker opened, and typing in a whole list asks nothing.
    const moveReads = () => trackerRequests(harness, 'GET rest/api/3/issue/{issue}/transitions');
    await press(status.getByTestId('ticket-field-value'), compact);
    await press(moves.filter({ hasText: 'Resolve' }), compact);
    await expect(form).toHaveAttribute('data-move', '41');
    const readsBefore = (await moveReads()).length;
    const resolution = field(form, 'resolution');
    await press(resolution.getByTestId('ticket-field-value'), compact);
    await expect(moves).toHaveText(['Done', 'Fixed']);
    await page.getByTestId('ticket-picker-find').pressSequentially('Fix');
    await expect(moves).toHaveText(['Fixed']);
    await press(moves, compact);
    await expect(resolution.getByTestId('ticket-field-value')).toHaveText('Fixed');
    expect((await moveReads()).length - readsBefore).toBe(1);
    const parts = field(form, 'customfield_10090');
    await press(parts.getByTestId('ticket-field-value'), compact);
    await press(moves.filter({ hasText: 'Web' }), compact);
    await press(moves.filter({ hasText: 'API' }), compact);
    await expect(moves.filter({ hasText: 'Web' })).toHaveAttribute('aria-checked', 'true');
    await expect(moves.filter({ hasText: 'API' })).toHaveAttribute('aria-checked', 'true');
    await page.keyboard.press('Escape');
    await expect(parts.getByTestId('ticket-field-value')).toHaveText('Web, API');
    expect((await moveReads()).length - readsBefore).toBe(2);
    const longText = (id: string) => form.locator(`[data-testid="ticket-move-text-field"][data-field="${id}"]`).getByTestId('ticket-move-text');
    await longText('customfield_10095').pressSequentially('A stale cache key');
    // The description starts from the one the ticket shows, layout and all;
    // the same text typed without the layout's markers is refused for the
    // layout it would lose.
    const moveDescription = longText('description');
    expect(await moveDescription.inputValue()).toContain('<!-- layout -->');
    await retype(moveDescription, 'Linked intro, resolved\n\nLeft side\n\nRight side');
    const sent = () => trackerRequests(harness, 'POST rest/api/3/issue/{issue}/transitions', ['WEB-6', '10106']);
    const anyway = ticket.getByTestId('ticket-write-save-anyway');
    await press(form.getByTestId('ticket-move-submit'), compact);
    await expect(anyway).toBeVisible({ timeout: 15_000 });
    // Changing the form withdraws Save anyway: what Move sends next is checked again.
    await longText('customfield_10095').pressSequentially(', checked again');
    await expect(anyway).toHaveCount(0);
    await press(form.getByTestId('ticket-move-submit'), compact);
    await expect(anyway).toBeVisible({ timeout: 15_000 });
    expect(await sent()).toEqual([]);
    // While the move is out (held by the fake), the form takes no change.
    const moveRoute = 'POST rest/api/3/issue/{issue}/transitions';
    const jira = { tracker: 'jira', site: JIRA_SITE };
    await harness.rpc('HarnessTrackerFault', { origin: jira, route: moveRoute, kind: 'hold', times: 1 });
    await press(anyway, compact);
    await expect.poll(async () => (await sent()).some((r) => r.held)).toBe(true);
    await expect(resolution.getByTestId('ticket-field-value')).toBeDisabled();
    await expect(parts.getByTestId('ticket-field-value')).toBeDisabled();
    await expect(longText('customfield_10095')).toBeDisabled();
    await expect(resolution.getByTestId('ticket-field-value')).toHaveText('Fixed');
    await harness.rpc('HarnessTrackerRelease', { origin: jira, route: moveRoute });
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    await expect(anyway).toHaveCount(0);
    await expect(status.getByTestId('ticket-field-value')).toContainText('Done');
    const [move] = await sent();
    expect(move.status).toBe(204);
    expect(move.body).toMatchObject({
      transition: { id: '41' },
      fields: { resolution: { id: '10001' }, customfield_10090: [{ id: '21001' }, { id: '21002' }] },
    });
    const moved = (move.body as { fields: Record<string, unknown> }).fields;
    expect(JSON.stringify(moved.customfield_10095)).toContain('A stale cache key, checked again');
    expect(JSON.stringify(moved.description)).toContain('Linked intro, resolved');
    await expect(ticket.locator('[data-testid="ticket-section"][data-field="description"]').getByTestId('ticket-section-text')).toContainText('Linked intro, resolved', { timeout: 15_000 });

    // Close's screen requires two fields Jira fills: the form asks for
    // neither, offers no None for the one it can set, and Move sends none.
    await press(status.getByTestId('ticket-field-value'), compact);
    await press(moves.filter({ hasText: 'Close' }), compact);
    await expect(form).toHaveAttribute('data-move', '61');
    await expect(form.getByTestId('ticket-move-unsettable')).toHaveCount(0);
    await expect(field(form, 'customfield_10097')).not.toContainText('(required)');
    const closeResolution = field(form, 'resolution');
    await expect(closeResolution).not.toContainText('(required)');
    await press(closeResolution.getByTestId('ticket-field-value'), compact);
    // Fixed, as Resolve set it, is checked; nothing clears it.
    await expect(moves).toHaveText([/^\s*Done\s*$/, /Fixed\s*✓/]);
    await expect(page.getByTestId('ticket-picker-clear')).toHaveCount(0);
    await page.keyboard.press('Escape');
    await press(form.getByTestId('ticket-move-submit'), compact);
    await expect(form).toHaveCount(0, { timeout: 15_000 });
    const closed = (await sent()).filter((r) => (r.body as { transition: { id: string } }).transition.id === '61');
    expect(closed).toHaveLength(1);
    expect(closed[0].status).toBe(204);
    expect(closed[0].body).toMatchObject({ transition: { id: '61' } });
    expect(Object.keys((closed[0].body as { fields?: object }).fields ?? {})).toEqual([]);
  });

  test('an older page of comments takes an edit and a delete, and what was typed outlasts a look at the history', async ({ harness, page }) => {
    test.setTimeout(120_000);
    await connectFakeJiraBoard(harness, compact ? 'compact-issues-ticket-pages' : 'issues-ticket-pages', EDITED);
    await harness.open(page);
    const pane = await openPane(page, compact);
    await expect(card(pane, '10107')).toContainText('Long discussion', { timeout: 30_000 });
    const ticket = await openTicket(pane, '10107', compact);
    const comments = ticket.getByTestId('ticket-comment');
    await expect(comments).toHaveCount(5, { timeout: 30_000 });

    // Jira's first page is the newest 50, read newest first; the comments
    // before them are read oldest first as a person reveals them.
    const pageReads = async () => (await trackerRequests(harness, 'GET rest/api/3/issue/{issue}/comment', ['WEB-7', '10107']))
      .filter((r) => r.query?.orderBy?.[0] === 'created');
    const older = ticket.locator('[data-testid="ticket-comment"][data-comment="50002"]');
    const earlier = ticket.getByTestId('ticket-comments-earlier');
    const readsAtOpen = (await pageReads()).length;
    // Each press mounts 20 more; the one that runs past the 50 held reads
    // the page before them.
    for (const shown of [25, 45]) {
      await press(earlier, compact);
      await expect(comments).toHaveCount(shown);
    }
    expect((await pageReads()).length).toBe(readsAtOpen);
    await press(earlier, compact);
    await expect(comments).toHaveCount(60);
    await expect(earlier).toHaveCount(0);
    await expect(older).toContainText('Seeded comment 2');
    expect((await pageReads()).length - readsAtOpen).toBe(1);

    // An edit there shows, re-reading that page once.
    let menu = await commentMenu(page, older, compact);
    await press(menu.getByTestId('ticket-comment-menu-edit'), compact);
    const editor = older.getByTestId('ticket-comment-editor');
    await retype(editor, 'Seeded comment 2, edited');
    const readsBeforeEdit = (await pageReads()).length;
    await press(older.getByTestId('ticket-comment-save'), compact);
    await expect(older.getByTestId('ticket-comment-body')).toHaveText('Seeded comment 2, edited', { timeout: 15_000 });
    expect((await trackerRequests(harness, 'PUT rest/api/3/issue/{issue}/comment/{id}')).map((r) => r.status)).toEqual([200]);
    await expect.poll(async () => (await pageReads()).length - readsBeforeEdit).toBe(1);

    // A delete there takes it out. Jira's first page now starts one
    // comment earlier, so the page held is read again from there, once.
    const deleted = ticket.locator('[data-testid="ticket-comment"][data-comment="50003"]');
    menu = await commentMenu(page, deleted, compact);
    await press(menu.getByTestId('ticket-comment-menu-delete'), compact);
    await press(deleted.getByTestId('ticket-comment-delete'), compact);
    await expect(deleted).toHaveCount(0, { timeout: 15_000 });
    expect((await trackerRequests(harness, 'DELETE rest/api/3/issue/{issue}/comment/{id}')).map((r) => r.status)).toEqual([204]);
    await expect.poll(async () => (await pageReads()).length - readsBeforeEdit).toBe(2);
    await expect(comments).toHaveCount(59);

    // A comment posted with every comment shown moves the first page one
    // on: the comment at the old boundary stays, read again with its page.
    const boundary = ticket.locator('[data-testid="ticket-comment"][data-comment="50011"]');
    await expect(boundary).toContainText('Seeded comment 11');
    await ticket.getByTestId('ticket-comment-input').pressSequentially('Posted with every comment shown');
    await press(ticket.getByTestId('ticket-comment-send'), compact);
    await expect(comments.filter({ hasText: 'Posted with every comment shown' })).toHaveCount(1, { timeout: 15_000 });
    await expect.poll(async () => (await pageReads()).length - readsBeforeEdit).toBe(3);
    await expect(comments).toHaveCount(60);
    await expect(boundary).toContainText('Seeded comment 11');
    await expect(ticket.getByTestId('ticket-comments-earlier')).toHaveCount(0);

    // Refresh brings neither back.
    const issueReads = () => trackerRequests(harness, 'GET rest/api/3/issue/{issue}', ['WEB-7', '10107']);
    const readsBeforeRefresh = (await issueReads()).length;
    await press(ticket.getByTestId('ticket-refresh'), compact);
    await expect.poll(async () => (await issueReads()).length).toBeGreaterThan(readsBeforeRefresh);
    await expect(ticket.getByTestId('ticket-refresh').locator('.animate-spin')).toHaveCount(0);
    await expect(older.getByTestId('ticket-comment-body')).toHaveText('Seeded comment 2, edited');
    await expect(deleted).toHaveCount(0);

    // A new comment and an edit, both unsent, outlast a look at the history.
    const box = ticket.getByTestId('ticket-comment-input');
    await box.pressSequentially('Typed before looking at the history');
    const newest = ticket.locator('[data-testid="ticket-comment"][data-comment="50060"]');
    menu = await commentMenu(page, newest, compact);
    await press(menu.getByTestId('ticket-comment-menu-edit'), compact);
    await retype(newest.getByTestId('ticket-comment-editor'), 'An edit typed before looking at the history');
    await press(ticket.getByRole('radio', { name: 'History' }), compact);
    const events = ticket.getByTestId('ticket-history-event');
    await expect(events).toHaveCount(20, { timeout: 15_000 });
    await press(ticket.getByTestId('ticket-history-earlier'), compact);
    await expect(events).toHaveCount(30);
    await press(ticket.getByRole('radio', { name: 'Comments' }), compact);
    await expect(box).toHaveValue('Typed before looking at the history');
    await expect(newest.getByTestId('ticket-comment-editor')).toHaveValue('An edit typed before looking at the history');
  });

  test('a ticket open on a paired computer that goes off says so where its content was, and shows again once it is back', async ({ page }) => {
    test.setTimeout(240_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-issues-ticket-'));
    const dataDir = join(root, 'state');
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    try {
      [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir }));
      const laptop = await pairOwnDevice(home, remote, 'Laptop');
      await connectFakeJiraBoard(remote, 'issues-ticket-remote', TICKET);
      await home.open(page);
      const pane = await openPane(page, compact);
      await expect(card(pane, ISSUE_ID)).toBeVisible({ timeout: 60_000 });
      const ticket = await openTicket(pane, ISSUE_ID, compact);
      const comments = ticket.getByTestId('ticket-comment');
      await expect(comments).toHaveCount(5, { timeout: 30_000 });
      await expect(ticket.getByTestId('ticket-offline')).toHaveCount(0);
      // A comment typed and not sent, which the computer going off keeps.
      await ticket.getByTestId('ticket-comment-input').pressSequentially('Typed before the computer went off');

      // Off: one sentence where the content was, under the card's title,
      // and none from the board the ticket covers.
      expect(await remote.stop()).toBe(true);
      await expect(ticket.getByTestId('ticket-offline')).toHaveText('Laptop is not connected. The ticket shows when it connects.', { timeout: 30_000 });
      await expect(ticket.getByTestId('ticket-title')).toHaveText('Ticket with comments');
      await expect(comments).toHaveCount(0);
      await expect(ticket.getByTestId('ticket-opening')).toHaveCount(0);
      await expect(pane.getByText('Laptop is not connected')).toHaveCount(1);

      // Back on, at its new address, with its Jira site served again: the
      // open ticket reads again.
      remote = await launchHarness({ dataDir });
      await seedFakeJira(remote, TICKET);
      await home.rpc('RepairBackendAddress', laptop, `https://127.0.0.1:${remote.bootstrap.port}`);
      await expect(comments).toHaveCount(5, { timeout: 60_000 });
      await expect(ticket.getByTestId('ticket-offline')).toHaveCount(0);
      await expect(ticket.getByTestId('ticket-comment-input')).toHaveValue('Typed before the computer went off');
    } finally {
      await remote?.close();
      await home?.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}
