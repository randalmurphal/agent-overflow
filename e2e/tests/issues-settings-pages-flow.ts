// Settings > Issues > Fields, Starting work and Agents (docs/specs/issues.md,
// "Setup"), shared by the desktop and compact projects, through the
// shipped UI, the real backend, the project's tracker.json, the fake
// trackers and mock provider sessions that call the tracker tools.
//
// Covered on both: a field pinned on the Fields page laying out the
// project's tickets, and a section pinned on a ticket listed on the page;
// the Starting work page's move, assignment and unnamed branch filling the
// Start work form, and the form's Save as defaults shown on the page; with
// Change tickets off, a thread's ticket change refused with the sentence
// the agent is told and nothing sent, and with Ask before changing
// several at once off, a change to two tickets sent with no prompt; a
// Linear-only project's Fields page saying it has nothing to pin; on each
// of the three pages, a save that lost to a hand edit of the file said in
// words with the hand edit shown and kept, a save whose outcome is unknown
// (the project's directory refuses the write) said in the computer's
// words, a file that cannot be read said with its path, and a section a
// newer build saved said with no control; and a site with 400 fields, of
// which the page draws none until a name is typed and at most 20 of those
// that match, with the time to show the page and to filter it recorded as
// annotations. On desktop: a connected browser's changes on each page
// saved, followed by this window's page, and a paired computer's project
// set on that computer.

import { chmod, mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

import type { Locator, Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { toolRow } from './ao-tool-rows-helpers.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import {
  EFFORT,
  PINNED_ID,
  ROOT_CAUSE,
  SETUP,
  card,
  closeRail,
  closeSettings,
  closeTicket,
  openBoard,
  openIssuesSetting,
  openRail,
  openTicket,
  railFields,
  sectionPin,
  setupFile,
  storedSetup,
  writeSetup,
} from './issues-setup-flow.js';
import { startTogether } from './launch-helpers.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { showColumn, workOf } from './ticket-work-flow.js';
import { TRACKER_TOOLS_SERVER, expandActivityRuns } from './tracker-agent-tools-helpers.js';
import { connectFakeJira, connectFakeJiraBoard, connectFakeLinearBoard, openIssuesPane, press, seedFakeJira, trackerRequests } from './tracker-helpers.js';

export interface SettingsPagesSurface {
  compact: boolean;
}

const COMMENT_ROUTE = 'POST rest/api/3/issue/{issue}/comment';
const WEB2 = '10102';

function fieldsPage(page: Page, compact: boolean): Promise<Locator> {
  return openIssuesSetting(page, compact, 'Fields', 'settings-tracker-fields');
}

function startWorkPage(page: Page, compact: boolean): Promise<Locator> {
  return openIssuesSetting(page, compact, 'Starting work', 'settings-tracker-start-work');
}

function agentsPage(page: Page, compact: boolean): Promise<Locator> {
  return openIssuesSetting(page, compact, 'Agents', 'settings-tracker-agents');
}

/** The Fields page's list of fields or of sections. */
function pinList(screen: Locator, kind: 'fields' | 'sections'): Locator {
  return screen.locator(`[data-testid="tracker-fields-list"][data-kind="${kind}"]`);
}

/** The Starting work page's rows for tracker project `project`. */
function place(screen: Locator, project: string): Locator {
  return screen.locator(`[data-testid="tracker-start-work-place"][data-project="${project}"]`);
}

function rule(screen: Locator, name: string): Locator {
  return screen.getByRole('switch', { name, exact: true });
}

/** One of the three pages, for what their shared frame does. */
interface PageCase {
  name: string;
  open: (page: Page, compact: boolean) => Promise<Locator>;
  /** The page's test ID stem. */
  id: string;
  section: string;
  /** What the page sets, as its sentences name it. */
  what: string;
  /** A hand edit of the file, for the site `site`. */
  handEdit: (site: string) => Record<string, unknown>;
  /** The control one change is made with. */
  control: (screen: Locator) => Locator;
  /** Expects the page to show the hand edit. */
  showsHandEdit: (screen: Locator) => Promise<void>;
  /** Draws the page's widest content, for the compact width check. */
  widest?: (screen: Locator) => Promise<void>;
}

/** A field whose name has no place to break, wider than a phone. */
const LONG_FIELD = {
  id: 'customfield_10777',
  name: `Customer${'_impact'.repeat(16)}`,
  schema: { type: 'string', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:textfield', customId: 10777 },
};

const CASES: PageCase[] = [
  {
    name: 'Fields',
    open: fieldsPage,
    id: 'fields',
    section: 'ticket_pins',
    what: 'pinned fields and sections',
    handEdit: (site) => ({ ticket_pins: { version: 1, sites: [{ tracker: 'jira', site, fields: [EFFORT] }] } }),
    control: (screen) => pinList(screen, 'sections').locator('[data-testid="tracker-fields-pinned"][data-id="description"]'),
    showsHandEdit: (screen) => expect(pinList(screen, 'fields').getByTestId('tracker-fields-pinned')).toHaveText(['Effort']),
    widest: async (screen) => {
      await pinList(screen, 'fields').getByTestId('tracker-fields-find').fill('customer_impact');
      await expect(pinList(screen, 'fields').getByTestId('tracker-fields-match')).toHaveText([LONG_FIELD.name]);
    },
  },
  {
    name: 'Starting work',
    open: startWorkPage,
    id: 'start-work',
    section: 'start_work',
    what: 'Start work defaults',
    handEdit: (site) => ({ start_work: { version: 1, projects: [{ tracker: 'jira', site, project: 'WEB', branch: 'web/{key}' }] } }),
    control: (screen) => rule(place(screen, 'WEB'), 'Assign it to me'),
    showsHandEdit: (screen) => expect(place(screen, 'WEB').getByTestId('tracker-start-work-branch-example')).toHaveText('web/WEB-123'),
  },
  {
    name: 'Agents',
    open: agentsPage,
    id: 'agents',
    section: 'agents',
    what: 'Agents settings',
    handEdit: () => ({ agents: { version: 1, ask_several: false } }),
    control: (screen) => rule(screen, 'Change tickets'),
    showsHandEdit: (screen) => expect(rule(screen, 'Ask before changing several at once')).toHaveAttribute('aria-checked', 'false'),
  },
];

/** The controls a page offers for a change: none while it cannot be read
 * or a newer build saved its section. */
function controls(screen: Locator): Locator {
  return screen.locator('[role="switch"], [data-testid="tracker-fields-pinned"], [data-testid="tracker-fields-find"]');
}

/** A project on the fake Jira site with one thread on the mock Claude,
 * whose turns are `turns`. */
async function seedAgentProject(harness: HarnessApp, tag: string, title: string, turns: Array<{ keys: string[]; comment: string }>) {
  await seedFakeJira(harness);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `settings-agents-${tag}`, repo: {}, threads: [{ title, provider: 'claude' }] }] });
  const project = seed.projects[0];
  await connectFakeJira(harness, project.projectId);
  await setScenario(harness, project.path, threadToolsScenario({
    name: `settings-agents-${tag}`, provider: 'claude',
    turns: turns.map((args) => ({ steps: [{ call: { server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', args } }], text: 'Done.' })),
    afterTurns: 'silent',
  }));
  return { path: project.path, projectId: project.projectId, threadId: project.threadIds[0] };
}

async function trackerAnswer(harness: HarnessApp, cwd: string): Promise<{ isError: boolean; text: string }> {
  const event = await harness.awaitMcpResult({ server: TRACKER_TOOLS_SERVER, tool: 'ticket_update', cwd, timeoutMs: 60_000 });
  return { isError: Boolean(event.report.isError), text: event.report.result ?? '' };
}

/** `count` fields named Field 1 to Field `count`, beside the fake's own. */
function manyFields(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    id: `customfield_${11001 + i}`,
    name: `Field ${i + 1}`,
    schema: { type: 'string', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:textfield', customId: 11001 + i },
  }));
}

export function registerIssuesSettingsPagesFlow({ compact }: SettingsPagesSurface): void {
  test('a field pinned on the Fields page lays out the project\'s tickets, and a section pinned on a ticket is listed on the page', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const { board, pane, file } = await openBoard(harness, page, 'settings-fields', compact);
    let screen = await fieldsPage(page, compact);
    // No section is pinned: the ones a ticket opens are listed as pinned.
    await expect(pinList(screen, 'sections').getByTestId('tracker-fields-pinned')).toHaveText(['Description', 'Linked tickets', 'Attachments'], { timeout: 15_000 });
    await expect(pinList(screen, 'fields').getByTestId('tracker-fields-pinned')).toHaveCount(0);
    await expect(screen.getByTestId('tracker-fields-match')).toHaveCount(0);
    await pinList(screen, 'fields').getByTestId('tracker-fields-find').fill('effo');
    await press(pinList(screen, 'fields').getByTestId('tracker-fields-match').filter({ hasText: /^Effort$/ }), compact);
    await expect(pinList(screen, 'fields').getByTestId('tracker-fields-pinned')).toHaveText(['Effort']);
    expect(await storedSetup(file)).toEqual({ ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, fields: [EFFORT] }] } });

    await closeSettings(page, compact);
    await openIssuesPane(page, compact);
    const ticket = await openTicket(pane, PINNED_ID, compact);
    await expect.poll(() => railFields(ticket, compact), { timeout: 15_000 }).toContain(EFFORT);
    await closeRail(ticket, compact);

    // Root cause, pinned from its header on the ticket.
    await press(ticket.getByTestId('ticket-more-sections'), compact);
    if (!compact) await ticket.locator(`[data-testid="ticket-section"][data-field="${ROOT_CAUSE}"] header`).hover();
    await press(sectionPin(ticket, ROOT_CAUSE), compact);
    await expect(ticket.getByTestId('ticket-more-sections')).toHaveCount(0);
    await closeTicket(pane, compact);
    screen = await fieldsPage(page, compact);
    await expect(pinList(screen, 'sections').getByTestId('tracker-fields-pinned')).toHaveText(['Description', 'Linked tickets', 'Attachments', 'Root cause']);
    await expect(pinList(screen, 'fields').getByTestId('tracker-fields-pinned')).toHaveText(['Effort']);
    expect(await storedSetup(file)).toEqual({
      ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, fields: [EFFORT], sections: ['description', 'links', 'attachments', ROOT_CAUSE] }] },
    });
  });

  test('the Starting work page fills the Start work form, and the form\'s Save as defaults shows on the page', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { board, pane, file } = await openBoard(harness, page, 'settings-start-work', compact);
    let screen = await startWorkPage(page, compact);
    let web = place(screen, 'WEB');
    await expect(rule(web, 'Move the ticket')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });
    await expect(web.getByTestId('tracker-start-work-branch-example')).toHaveText('WEB-123-short-title');
    await press(rule(web, 'Move the ticket'), compact);
    await expect(web.getByTestId('tracker-start-work-status')).toHaveValue('10001');
    await press(rule(web, 'Assign it to me'), compact);
    await expect(rule(web, 'Assign it to me')).toHaveAttribute('aria-checked', 'false');
    await press(rule(web, 'Name the branch after the ticket'), compact);
    await expect(web.getByTestId('tracker-start-work-branch-example')).toHaveText('Start work opens with the branch box empty, for you to name it.');
    expect(await storedSetup(file)).toEqual({ start_work: { version: 1, projects: [{ tracker: 'jira', site: board.site, project: 'WEB', status: '10001', assign: false, name_branch: false }] } });

    await closeSettings(page, compact);
    await openIssuesPane(page, compact);
    await showColumn(pane, 'To Do', compact);
    const ticket = await openTicket(pane, WEB2, compact);
    const work = await workOf(ticket, compact);
    await press(work.getByTestId('ticket-work-start'), compact);
    const form = page.getByTestId('start-work-form');
    await expect(form).toBeVisible();
    await expect(form.getByTestId('start-work-status').locator('option:checked')).toHaveText('In Progress');
    await expect(form.getByTestId('start-work-assign')).not.toBeChecked();
    await expect(form.getByTestId('start-work-branch')).toHaveValue('');
    await expect(form.getByTestId('start-work-go')).toBeDisabled();

    await form.getByTestId('start-work-branch').fill('web-2-spike');
    await press(form.getByTestId('start-work-assign'), compact);
    await press(form.getByTestId('start-work-save'), compact);
    await press(form.getByTestId('start-work-go'), compact);
    await expect(page.getByTestId('chat-header-title')).toHaveText('WEB-2 Second task', { timeout: 30_000 });

    screen = await startWorkPage(page, compact);
    web = place(screen, 'WEB');
    await expect(rule(web, 'Assign it to me')).toHaveAttribute('aria-checked', 'true', { timeout: 15_000 });
    await expect(web.getByTestId('tracker-start-work-status')).toHaveValue('10001');
    await expect(web.getByTestId('tracker-start-work-branch-example')).toHaveText('Start work opens with the branch box empty, for you to name it.');
    const stored = await storedSetup(file) as { start_work: { projects: unknown[] } };
    expect(stored.start_work.projects).toEqual([expect.objectContaining({ project: 'WEB', status: '10001', assign: true, name_branch: false })]);
  });

  test('with Change tickets off a thread\'s change is refused with what to do, and with Ask before changing several off two tickets go with no prompt', async ({ harness, page }) => {
    test.setTimeout(240_000);
    const title = 'Ticket changer';
    const { path, projectId, threadId } = await seedAgentProject(harness, compact ? 'compact' : 'desktop', title, [
      { keys: ['WEB-1'], comment: 'Looked at this.' },
      { keys: ['WEB-1', 'WEB-2'], comment: 'Both in this sprint.' },
    ]);
    const file = await setupFile(harness, projectId);
    await harness.open(page);
    const screen = await agentsPage(page, compact);
    await expect(rule(screen, 'Read tickets')).toBeDisabled({ timeout: 15_000 });
    await expect(rule(screen, 'Change tickets')).toHaveAttribute('aria-checked', 'true');
    await press(rule(screen, 'Change tickets'), compact);
    await expect(rule(screen, 'Change tickets')).toHaveAttribute('aria-checked', 'false');
    expect(await storedSetup(file)).toEqual({ agents: { version: 1, change_tickets: false } });

    await harness.send(threadId, 'comment on WEB-1');
    const refused = await trackerAnswer(harness, path);
    expect(refused).toEqual({
      isError: true,
      text: "[tracker_changes_off] This project's Agents settings do not let its threads change tickets, so nothing was sent; ask the user to turn on Change tickets in Settings > Issues > Agents if this change is wanted.",
    });
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(0);

    await press(rule(screen, 'Change tickets'), compact);
    await expect(rule(screen, 'Change tickets')).toHaveAttribute('aria-checked', 'true');
    await press(rule(screen, 'Ask before changing several at once'), compact);
    await expect(rule(screen, 'Ask before changing several at once')).toHaveAttribute('aria-checked', 'false');
    expect(await storedSetup(file)).toEqual({ agents: { version: 1, ask_several: false } });

    await harness.send(threadId, 'comment on both');
    const sent = await trackerAnswer(harness, path);
    expect(sent.isError, sent.text).toBe(false);
    expect(JSON.parse(sent.text)).toEqual({ tickets: [{ key: 'WEB-1', applied: ['comment'] }, { key: 'WEB-2', applied: ['comment'] }] });
    expect(await trackerRequests(harness, COMMENT_ROUTE)).toHaveLength(2);

    // The thread's row says why the first change was refused, and no prompt waits.
    await closeSettings(page, compact);
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') await page.getByTestId('compact-back').click();
    await press(page.getByTestId('thread-row-title').filter({ hasText: title }).first(), compact);
    await expandActivityRuns(page);
    await expect(toolRow(page, 'update', 'WEB-1').first().getByTestId('row-error-code').first()).toHaveText('tracker_changes_off', { timeout: 30_000 });
    await expect(page.getByTestId('composer-pending-approval')).toHaveCount(0);
  });

  test('a Linear-only project\'s Fields page says it has nothing to pin', async ({ harness, page }) => {
    await connectFakeLinearBoard(harness, 'settings-linear', []);
    await harness.open(page);
    const screen = await fieldsPage(page, compact);
    await expect(screen.getByTestId('settings-tracker-fields-none')).toHaveText(
      "A Linear ticket's fields are fixed, so this project has no fields to pin. Its sections are pinned on the ticket.",
      { timeout: 15_000 },
    );
    await expect(screen.getByTestId('tracker-fields-list')).toHaveCount(0);
  });

  for (const c of CASES) {
    test(`the ${c.name} page says a save that lost to a hand edit, one whose outcome is unknown, a file that cannot be read and a section a newer build saved`, async ({ harness, page }) => {
      test.setTimeout(180_000);
      const { board, file } = await openBoard(harness, page, `settings-frame-${c.id}`, compact, { ...SETUP, fields: [...SETUP.fields!, LONG_FIELD] });
      let screen = await c.open(page, compact);
      await expect(c.control(screen)).toBeVisible({ timeout: 15_000 });
      if (compact) {
        // Every control within the phone's width, the widest content drawn.
        await c.widest?.(screen);
        const width = page.viewportSize()!.width;
        const outside = await screen.locator('button, input, select, [role="switch"]').evaluateAll(
          (els, w) => els.filter((el) => el.getBoundingClientRect().right > w).map((el) => el.outerHTML.slice(0, 80)),
          width,
        );
        expect(outside).toEqual([]);
      }

      // Lost to a hand edit: said, the hand edit shown and kept.
      await writeSetup(file, JSON.stringify(c.handEdit(board.site)));
      await press(c.control(screen), compact);
      await expect(screen.getByTestId(`settings-tracker-${c.id}-save-note`)).toHaveText(
        "This change was not saved: the project's tracker setup changed after it was read; it is shown as it is now. Make the change again.",
      );
      await c.showsHandEdit(screen);
      expect(await storedSetup(file)).toEqual(c.handEdit(board.site));

      // The project's directory refuses the write: the outcome is unknown.
      const dir = dirname(file);
      await chmod(dir, 0o500);
      try {
        await press(c.control(screen), compact);
        const note = screen.getByTestId(`settings-tracker-${c.id}-save-note`);
        await expect(note).toContainText('This change may or may not be stored: the computer that holds the project could not finish updating');
        await expect(note).toContainText(file);
      } finally {
        await chmod(dir, 0o700);
      }
      await c.showsHandEdit(screen);
      expect(await storedSetup(file)).toEqual(c.handEdit(board.site));

      // A file that cannot be read: the computer holds the file it read
      // until a save reads it again, which fails and says so; the page
      // shown again says it with Try again and no control.
      await writeSetup(file, `{"${c.section}": `);
      await press(c.control(screen), compact);
      await expect(screen.getByTestId(`settings-tracker-${c.id}-save-note`)).toContainText(file);
      expect(await readFile(file, 'utf8')).toBe(`{"${c.section}": `);
      await page.reload();
      screen = await c.open(page, compact);
      await expect(screen.getByTestId(`settings-tracker-${c.id}-error`)).toContainText(file, { timeout: 15_000 });
      await expect(controls(screen)).toHaveCount(0);

      // A section a newer build saved.
      await writeSetup(file, JSON.stringify({ [c.section]: { version: 2, rules: ['any'] } }));
      await press(screen.getByTestId(`settings-tracker-${c.id}-retry`), compact);
      await expect(screen.getByTestId(`settings-tracker-${c.id}-newer`)).toContainText(`A newer version of Agent Overflow saved this project's ${c.what}.`);
      await expect(screen.getByTestId(`settings-tracker-${c.id}-error`)).toHaveCount(0);
      await expect(controls(screen)).toHaveCount(0);
    });
  }

  test('a site with 400 fields shows the pinned at once and draws at most 20 of the fields that match a name', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const count = 400;
    const board = await connectFakeJiraBoard(harness, 'settings-fields-many', { ...SETUP, fields: [...SETUP.fields!, ...manyFields(count)] });
    const file = await setupFile(harness, board.projectId);
    await writeSetup(file, JSON.stringify({ ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, fields: ['customfield_11007', 'customfield_11003'] }] } }));
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });

    // From the tab's press to the pinned drawn: the fields read on the
    // fake site and the page's draw.
    const started = Date.now();
    const screen = await fieldsPage(page, compact);
    const fields = pinList(screen, 'fields');
    await expect(fields.getByTestId('tracker-fields-pinned')).toHaveText(['Field 7', 'Field 3'], { timeout: 15_000 });
    const shownMs = Date.now() - started;
    await expect(screen.getByTestId('tracker-fields-match')).toHaveCount(0);
    const elements = await screen.locator('*').count();

    // From the typed name to its matches drawn, in the page.
    const find = fields.getByTestId('tracker-fields-find');
    const filterMs = await find.evaluate(async (input: HTMLInputElement) => {
      const start = performance.now();
      input.value = 'field 1';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      return performance.now() - start;
    });
    // Field 1, 10 to 19 and 100 to 199 match: 111, of which 20 draw.
    await expect(fields.getByTestId('tracker-fields-match')).toHaveCount(20);
    await expect(fields.getByTestId('tracker-fields-more')).toHaveText('91 more match; type more of the name.');
    const filteredElements = await screen.locator('*').count();

    await find.fill('field 123');
    await press(fields.getByTestId('tracker-fields-match').filter({ hasText: /^Field 123$/ }), compact);
    await expect(fields.getByTestId('tracker-fields-pinned')).toHaveText(['Field 7', 'Field 3', 'Field 123']);

    const measured = `${count} fields: page shown in ${shownMs} ms with ${elements} elements; "field 1" filtered and drawn in ${filterMs.toFixed(1)} ms with ${filteredElements} elements`;
    test.info().annotations.push({ type: 'fields-page', description: measured });
    console.log(`[fields-page ${compact ? 'compact' : 'desktop'}] ${measured}`);
  });

  if (!compact) {
    test('a connected browser\'s changes on each page are saved, and this window\'s page follows them', async ({ browser, page }) => {
      test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a connected browser');
      test.setTimeout(180_000);
      const harness = await launchHarness();
      const context = await browser.newContext();
      try {
        await harness.rpc('SetNetworkSettings', { bindAll: true });
        const board = await connectFakeJiraBoard(harness, 'settings-connected', SETUP);
        const file = await setupFile(harness, board.projectId);
        await harness.open(page);
        const pane = await openIssuesPane(page, false);
        await expect(card(pane, '10101')).toBeVisible({ timeout: 30_000 });
        const here = await agentsPage(page, false);
        await expect(rule(here, 'Change tickets')).toHaveAttribute('aria-checked', 'true', { timeout: 15_000 });

        const device = await context.newPage();
        const code = await redeemOnScreen(device, await mintInvite(harness, 'full'), 'Settings browser');
        await confirmOnHost(harness, code);
        const agents = await agentsPage(device, false);
        await press(rule(agents, 'Change tickets'), false);
        await expect(rule(agents, 'Change tickets')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });
        await expect(rule(here, 'Change tickets')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });

        const start = await startWorkPage(device, false);
        await press(rule(place(start, 'WEB'), 'Assign it to me'), false);
        await expect(rule(place(start, 'WEB'), 'Assign it to me')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });
        const pins = await fieldsPage(device, false);
        await press(pinList(pins, 'sections').locator('[data-testid="tracker-fields-pinned"][data-id="description"]'), false);
        await expect(pinList(pins, 'sections').getByTestId('tracker-fields-pinned')).toHaveText(['Linked tickets', 'Attachments'], { timeout: 15_000 });
        expect(await storedSetup(file)).toEqual({
          ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, sections: ['links', 'attachments'] }] },
          start_work: { version: 1, projects: [{ tracker: 'jira', site: board.site, project: 'WEB', assign: false }] },
          agents: { version: 1, change_tickets: false },
        });
        const there = await fieldsPage(page, false);
        await expect(pinList(there, 'sections').getByTestId('tracker-fields-pinned')).toHaveText(['Linked tickets', 'Attachments'], { timeout: 15_000 });
      } finally {
        await context.close();
        await harness.close();
      }
    });

    test('a paired computer\'s project is set on each page on that computer', async ({ page }) => {
      test.setTimeout(240_000);
      const root = await mkdtemp(join(tmpdir(), 'ao-issues-settings-'));
      let home: HarnessApp | undefined;
      let remote: HarnessApp | undefined;
      try {
        [home, remote] = await startTogether(launchHarness(), launchHarness({ dataDir: join(root, 'state') }));
        await pairOwnDevice(home, remote, 'Laptop');
        const board = await connectFakeJiraBoard(remote, 'settings-remote', SETUP);
        const file = await setupFile(remote, board.projectId);
        await home.open(page);
        const pane = await openIssuesPane(page, false);
        await expect(card(pane, '10101')).toBeVisible({ timeout: 60_000 });

        const onLaptop = async (open: (page: Page, compact: boolean) => Promise<Locator>) => {
          const screen = await open(page, false);
          await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption({ label: 'Laptop' });
          return screen;
        };
        const agents = await onLaptop(agentsPage);
        await press(rule(agents, 'Change tickets'), false);
        await expect(rule(agents, 'Change tickets')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });
        const start = await onLaptop(startWorkPage);
        await press(rule(place(start, 'WEB'), 'Assign it to me'), false);
        await expect(rule(place(start, 'WEB'), 'Assign it to me')).toHaveAttribute('aria-checked', 'false', { timeout: 15_000 });
        const pins = await onLaptop(fieldsPage);
        await pinList(pins, 'fields').getByTestId('tracker-fields-find').fill('effo');
        await press(pinList(pins, 'fields').getByTestId('tracker-fields-match').filter({ hasText: /^Effort$/ }), false);
        await expect(pinList(pins, 'fields').getByTestId('tracker-fields-pinned')).toHaveText(['Effort'], { timeout: 15_000 });
        expect(await storedSetup(file)).toEqual({
          ticket_pins: { version: 1, sites: [{ tracker: 'jira', site: board.site, fields: [EFFORT] }] },
          start_work: { version: 1, projects: [{ tracker: 'jira', site: board.site, project: 'WEB', assign: false }] },
          agents: { version: 1, change_tickets: false },
        });
        await closeSettings(page, false);
        const ticket = await openTicket(pane, PINNED_ID, false);
        await expect.poll(() => railFields(ticket, false), { timeout: 15_000 }).toContain(EFFORT);
        await openRail(ticket, false);
      } finally {
        await remote?.close();
        await home?.close();
        await rm(root, { recursive: true, force: true });
      }
    });
  }
}
