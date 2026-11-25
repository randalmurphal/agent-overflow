// A ticket in a pane of its own (docs/specs/issues.md, "Pane"), on the
// desktop project, through the shipped UI, the real backend and the fake
// Jira. Covered: a card pressed with Ctrl or Cmd opens its ticket in a
// pane right of the Issues pane, focused, the Issues pane keeping its
// board; a linked ticket opened in the pane takes its place, Escape steps
// back to the ticket with what was typed on it, and at the first ticket
// leaves the pane open; a link pressed with Ctrl or Cmd opens another
// pane, whose close releases its subscription on the wire; a reload brings
// the pane back on its ticket. In its own pane in a ticket's menu moves
// the ticket open in the Issues pane, with what was typed on it, without
// subscribing it again, and the board shows. A closed pane gives back
// what it held: its subscription, its DOM and its heap.
//
// The compact layout offers no ticket pane (compact-issues-ticket.spec.ts
// and IssuesPane.test.ts); the move's hold, drafts and a restore that
// does not read back are IssuesPane.test.ts's.

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { methodIdByName } from './offhost-helpers.js';
import { connectFakeJiraBoard, openIssuesPane, type ExtraJira } from './tracker-helpers.js';

// WEB-6 blocks WEB-1 and is blocked by WEB-2; WEB-5 has no links.
const LINKED: ExtraJira = {
  projects: [],
  boards: [],
  links: [{ id: '40001', type: '10000', outward: '10106', inward: '10101' }, { id: '40002', type: '10000', outward: '10102', inward: '10106' }],
  issues: [
    { id: '10105', key: 'WEB-5', project: 'WEB', type: '10002', status: '10001', summary: 'Moved into a pane', sprints: [1] },
    { id: '10106', key: 'WEB-6', project: 'WEB', type: '10002', status: '10001', summary: 'Linked both ways', sprints: [1] },
  ],
};

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

/** Counts the calls of `method` this page sends, from its first socket on. */
function sentCalls(page: Page, method: string): () => number {
  const id = methodIdByName(method);
  let count = 0;
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      if ((JSON.parse(payload) as { methodId?: number }).methodId === id) count += 1;
    });
  });
  return () => count;
}

test.beforeEach(async ({ page }) => {
  await page.setViewportSize({ width: 1800, height: 900 });
});

test('a ticket opens in a pane of its own from a card and a link, steps back on Escape, comes back after a reload, and its close lets go of it', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const subscribes = sentCalls(page, 'SubscribeTrackerTicket');
  const releases = sentCalls(page, 'ReleaseTrackerSubscription');
  await connectFakeJiraBoard(harness, 'issues-ticket-pane', LINKED);
  await harness.open(page);
  const issues = await openIssuesPane(page, false);
  await expect(card(issues, '10106')).toBeVisible({ timeout: 30_000 });

  await card(issues, '10106').getByTestId('issues-card-open').click({ modifiers: ['ControlOrMeta'] });
  const own = page.getByTestId('ticket-pane');
  await expect(own).toHaveCount(1);
  const ticket = own.getByTestId('ticket-screen');
  await expect(ticket).toHaveAttribute('data-issue', '10106');
  await expect(ticket).toBeFocused();
  await expect(ticket.getByTestId('ticket-title')).toHaveText('Linked both ways');
  // Right of the Issues pane, which keeps its board.
  await expect(issues.getByTestId('ticket-screen')).toHaveCount(0);
  expect((await own.boundingBox())!.x).toBeGreaterThan((await issues.boundingBox())!.x);

  // A linked ticket opened in the pane takes its place; Escape steps back
  // to the ticket with what was typed on it, and then leaves the pane.
  await ticket.getByTestId('ticket-comment-input').fill('Unsent in the pane');
  await ticket.getByTestId('ticket-link').filter({ hasText: 'WEB-1' }).click();
  await expect(ticket).toHaveAttribute('data-issue', '10101');
  await expect(ticket.getByTestId('ticket-back')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(ticket).toHaveAttribute('data-issue', '10106');
  await expect(ticket).toBeFocused();
  await expect(ticket.getByTestId('ticket-comment-input')).toHaveValue('Unsent in the pane');
  await page.keyboard.press('Escape');
  await expect(ticket).toHaveAttribute('data-issue', '10106');
  await expect(own).toHaveCount(1);

  // A link pressed with Ctrl or Cmd opens a pane of its own; its close
  // releases its subscription.
  const subscribedBefore = subscribes();
  await ticket.getByTestId('ticket-link').filter({ hasText: 'WEB-2' }).click({ modifiers: ['ControlOrMeta'] });
  await expect(own).toHaveCount(2);
  const second = own.nth(1).getByTestId('ticket-screen');
  await expect(second).toHaveAttribute('data-issue', '10102');
  await expect(second).toBeFocused();
  await expect.poll(subscribes).toBe(subscribedBefore + 1);
  const releasedBefore = releases();
  await second.getByTestId('ticket-close').click();
  await expect(own).toHaveCount(1);
  await expect.poll(releases).toBe(releasedBefore + 1);

  // A reload brings the pane back on its ticket.
  await page.reload();
  await expect(page.getByTestId('issues-pane')).toBeVisible({ timeout: 30_000 });
  await expect(own).toHaveCount(1);
  await expect(own.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10106', { timeout: 30_000 });
  await expect(own.getByTestId('ticket-title')).toHaveText('Linked both ways');
  const releasedAfterReload = releases();
  await own.getByTestId('ticket-close').click();
  await expect(own).toHaveCount(0);
  await expect.poll(releases).toBe(releasedAfterReload + 1);
});

test('the pane-navigation chord moves the keyboard focus into a ticket pane while it loads and once it shows a short ticket', async ({ harness, page }) => {
  test.setTimeout(120_000);
  // Holds the ticket pane's code, so the pane is still loading when the
  // chord reaches it.
  let releaseBody: (() => void) | undefined;
  const bodyHeld = new Promise<void>((resolve) => { releaseBody = resolve; });
  await page.route(/\/TicketPaneBody-[^/]*\.js$/, async (route) => {
    await bodyHeld;
    await route.continue();
  });
  await connectFakeJiraBoard(harness, 'issues-ticket-pane-nav', LINKED, [{
    title: 'Nav source', provider: 'claude', turns: [{ userText: 'Earlier prompt', items: [{ kind: 'assistant_text', summary: 'Earlier answer' }] }],
  }]);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Nav source' }).click();
  const source = page.locator('section[data-pane-kind="thread"]');
  const composer = source.getByLabel('Message Input');
  await expect(composer).toBeVisible();
  // Beside the thread, not in its place.
  await page.getByTestId('sidebar-tool-issues').click({ modifiers: ['ControlOrMeta'] });
  const issues = page.getByTestId('issues-pane');
  await expect(card(issues, '10105')).toBeVisible({ timeout: 30_000 });
  await card(issues, '10105').getByTestId('issues-card-open').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.locator('section[data-pane-id]')).toHaveCount(3);
  const ticketPane = page.locator('section[data-pane-kind="ticket"]');
  await expect(ticketPane).toHaveCount(1);
  const focusInTicketPane = () => page.evaluate(() => !!document.activeElement?.closest('section[data-pane-kind="ticket"]'));

  // Loading: the chord from the composer, through the Issues pane.
  await composer.click();
  await composer.fill('kept draft');
  await page.keyboard.press('Alt+l');
  await page.keyboard.press('Alt+l');
  await expect(ticketPane).toHaveAttribute('data-pane-focused', 'true');
  await expect.poll(focusInTicketPane).toBe(true);
  await page.keyboard.type('stray');
  await expect(composer).toHaveValue('kept draft');

  // Shown, a ticket whose content fits: back to the composer, and over again.
  releaseBody!();
  await expect(ticketPane.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10105');
  await expect(ticketPane.getByTestId('ticket-title')).toHaveText('Moved into a pane');
  await composer.click();
  await page.keyboard.press('Alt+l');
  await page.keyboard.press('Alt+l');
  await expect(ticketPane).toHaveAttribute('data-pane-focused', 'true');
  await expect.poll(focusInTicketPane).toBe(true);
  await page.keyboard.type('stray');
  await expect(composer).toHaveValue('kept draft');
});

test('In its own pane moves the ticket open in the Issues pane, with what was typed on it, and the board shows again', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const subscribes = sentCalls(page, 'SubscribeTrackerTicket');
  const releases = sentCalls(page, 'ReleaseTrackerSubscription');
  await connectFakeJiraBoard(harness, 'issues-ticket-pane-move', LINKED);
  await harness.open(page);
  const issues = await openIssuesPane(page, false);
  await expect(card(issues, '10105')).toBeVisible({ timeout: 30_000 });
  await card(issues, '10105').getByTestId('issues-card-open').click();
  const here = issues.getByTestId('ticket-screen');
  await expect(here).toHaveAttribute('data-issue', '10105');
  await here.getByTestId('ticket-comment-input').fill('Typed before the move');
  await expect.poll(subscribes).toBe(1);
  const released = releases();

  await here.getByTestId('ticket-menu-button').click();
  await page.getByRole('menuitem', { name: 'In its own pane' }).click();
  const moved = page.getByTestId('ticket-pane').getByTestId('ticket-screen');
  await expect(moved).toHaveAttribute('data-issue', '10105');
  await expect(moved).toBeFocused();
  await expect(moved.getByTestId('ticket-comment-input')).toHaveValue('Typed before the move');
  await expect(here).toHaveCount(0);
  await expect(issues.getByTestId('issues-board-region')).not.toHaveAttribute('inert');
  // Held across the move: never subscribed again, never released.
  expect(subscribes()).toBe(1);
  expect(releases()).toBe(released);
});

test('a closed ticket pane gives back its subscription, its DOM and its heap', async ({ harness, page, context }) => {
  test.setTimeout(180_000);
  const releases = sentCalls(page, 'ReleaseTrackerSubscription');
  await connectFakeJiraBoard(harness, 'issues-ticket-pane-memory', LINKED);
  await harness.open(page);
  const issues = await openIssuesPane(page, false);
  await expect(card(issues, '10105')).toBeVisible({ timeout: 30_000 });
  const own = page.getByTestId('ticket-pane');
  const cdp = await context.newCDPSession(page);
  // What the page holds: the heap snapshot's objects, less the code V8
  // compiles as functions grow hot (it grows with every repeat of any
  // action and is no one's to give back), and the panes' DOM.
  const measure = async () => {
    await cdp.send('HeapProfiler.collectGarbage');
    const chunks: string[] = [];
    const onChunk = (e: { chunk: string }) => { chunks.push(e.chunk); };
    cdp.on('HeapProfiler.addHeapSnapshotChunk', onChunk);
    await cdp.send('HeapProfiler.takeHeapSnapshot', { reportProgress: false });
    cdp.off('HeapProfiler.addHeapSnapshotChunk', onChunk);
    const snap = JSON.parse(chunks.join('')) as { snapshot: { meta: { node_fields: string[]; node_types: [string[]] } }; nodes: number[] };
    const fields = snap.snapshot.meta.node_fields;
    const typeAt = fields.indexOf('type');
    const sizeAt = fields.indexOf('self_size');
    const code = snap.snapshot.meta.node_types[0].indexOf('code');
    let held = 0;
    for (let i = 0; i < snap.nodes.length; i += fields.length) if (snap.nodes[i + typeAt] !== code) held += snap.nodes[i + sizeAt];
    const nodes = await page.evaluate(() => document.querySelector('[data-testid="pane-strip"]')!.getElementsByTagName('*').length);
    return { heldKB: Math.round(held / 1024), nodes };
  };
  const openAndClose = async (issueId: string) => {
    await card(issues, issueId).getByTestId('issues-card-open').click({ modifiers: ['ControlOrMeta'] });
    await expect(own.getByTestId('ticket-screen')).toHaveAttribute('data-issue', issueId);
    await expect(own.getByTestId('ticket-comment-input')).toBeVisible();
    const open = await measure();
    const released = releases();
    await own.getByTestId('ticket-close').click();
    await expect(own).toHaveCount(0);
    await expect.poll(releases).toBe(released + 1);
    return open;
  };

  // A first pane on each ticket loads the ticket's code and runs each of
  // its parts once; then four panes open and close on them in turn.
  await openAndClose('10105');
  await openAndClose('10106');
  const before = await measure();
  const opens = [];
  for (const issueId of ['10106', '10105', '10106', '10105']) opens.push(await openAndClose(issueId));
  const after = await measure();
  const cost = `before ${JSON.stringify(before)}; each pane open ${JSON.stringify(opens)}; after four closed ${JSON.stringify(after)}`;
  test.info().annotations.push({ type: 'ticket pane cost', description: cost });
  console.log(`ticket pane cost: ${cost}`);
  expect(after.nodes).toBe(before.nodes);
  for (const open of opens) expect(open.nodes).toBeGreaterThan(before.nodes);
  // Four panes come and go and the page holds what it held before, give
  // or take less than half of what one open pane holds.
  const onePane = Math.min(...opens.map((open) => open.heldKB - before.heldKB));
  expect(after.heldKB - before.heldKB).toBeLessThan(onePane / 2);
  await cdp.detach();
});
