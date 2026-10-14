// The activity rail's Agents segment (docs/specs/standing-agents.md,
// "Agents segment"), end to end on both mock providers.
//
// Coverage: a spawn appears as a running row with its title and provider; a
// send with notify appears; the spawn's own spawn nests under it; a row
// blocked on an approval says so, and runs again once it is answered; a
// settled row shows its outcome, lingers and leaves; a click opens the
// target in a pane; a row's Stop cancels the request for the user, keeps
// the caller's wake armed with the user named, interrupts the spawned
// child, stops the child's background shell and cascades into the
// grandchild, whose own caller is woken the same way; Stop All cascades;
// Background's Stop All leaves the rows; and the thread's own Stop does not
// cascade. The cross-computer rows are thread-tools-paired.spec.ts's.
import { test, expect } from './fixtures.js';
import { advanceGate, awaitGate, type ProviderName } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import {
  CHILD,
  GRAND,
  LEAD,
  PEER,
  agentRow,
  approvalTool,
  agentsBody,
  ledgerRow,
  mockInput,
  mockInterrupt,
  openAgents,
  seedAgentsTree,
  startTree,
} from './agents-tray-helpers.js';

const USER_STOP = 'The user stopped it from the Agents tray';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: rows list, nest, block, settle and leave; a row's Stop cascades into what the child runs`, async ({ harness, page }) => {
    test.setTimeout(300_000);
    const tree = await seedAgentsTree(harness, provider, `agents-${provider}`);
    const approvalSeen = harness.waitForEvent<{ action: string; request?: { requestId: string; threadId: string; toolName: string } }>(
      'provider:approval', (ev) => ev.action === 'request' && ev.request?.toolName === approvalTool(provider), 90_000);
    const started = await startTree(harness, tree);
    const approval = await approvalSeen;
    expect(approval.request!.threadId).toBe(started.grand);
    // A seeded thread is listed once it has a message.
    await harness.open(page);

    const toggle = await openAgents(page, LEAD);
    const child = agentRow(page, started.childToken);
    const grand = agentRow(page, started.grandToken);
    const send = agentRow(page, started.sendToken);
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveText('3');
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(child.getByTestId('agent-tray-row-title')).toHaveText(CHILD);
    await expect(child.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    await expect(child.getByTestId('agent-tray-row-state')).toHaveText('running');
    await expect(child).toHaveAttribute('data-depth', '0');
    await expect(grand).toHaveAttribute('data-depth', '1');
    await expect(grand.getByTestId('agent-tray-row-title')).toHaveText(GRAND);
    await expect(grand.getByTestId('agent-tray-row-state')).toHaveText('blocked');
    await expect(grand.getByTestId('agent-tray-row-blocked')).toContainText(`Needs approval for ${approvalTool(provider)}`);
    await expect(send.getByTestId('agent-tray-row-title')).toHaveText(PEER);
    await expect(send).toHaveAttribute('data-kind', 'send');
    await capture(page, `agents-${provider}-running-blocked-nested.png`, agentsBody(page));

    // Answering the approval lets the grandchild run again.
    await harness.rpc('RespondToApproval', started.grand, { requestId: approval.request!.requestId, decision: 'allow' });
    await awaitGate(harness, 'grand-hold', tree.grandPath);
    await expect(grand.getByTestId('agent-tray-row-state')).toHaveText('running');

    // The peer answers: its row shows the outcome, lingers, then leaves.
    const peerHold = await awaitGate(harness, 'peer-hold', tree.peerPath);
    await advanceGate(harness, peerHold.mockId, 'peer-hold');
    await expect(send).toHaveAttribute('data-settled', 'true', { timeout: 30_000 });
    await expect(send.getByTestId('agent-tray-row-state')).toHaveText(/finished|replied/);
    await capture(page, `agents-${provider}-settled.png`, agentsBody(page));
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveText('2');
    const settledAt = Date.now();
    await expect(send).toHaveCount(0, { timeout: 20_000 });
    expect(Date.now() - settledAt, 'the settled row lingered').toBeGreaterThan(5_000);

    // A click opens the child in a pane.
    await child.getByTestId('agent-tray-row-open').click();
    await expect(page.getByTestId('chat-header-title').filter({ hasText: CHILD })).toBeVisible();
    await page.getByTestId('thread-row-title').getByText(LEAD, { exact: true }).click();
    await expect(agentsBody(page)).toBeVisible();

    // The child's background shell is listed in its own Backgrounded set.
    await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', started.child))
      .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(1);

    // Stop on the child's row: the request is cancelled for the user, the
    // child's turn is interrupted, its shell stopped, and the grandchild's
    // request cancelled and its turn interrupted; each caller is woken with
    // the user named.
    const childInterrupted = mockInterrupt(harness, tree.childPath);
    const grandInterrupted = mockInterrupt(harness, tree.grandPath);
    const leadWoken = mockInput(harness, tree.leadPath, [started.childToken, USER_STOP]);
    const childWoken = mockInput(harness, tree.childPath, [started.grandToken, USER_STOP]);
    await child.getByTestId('agent-tray-row-stop').click();
    await expect(page.getByTestId('toast').filter({ hasText: `Stopped ${CHILD}: its turn, 1 background task and 1 agent of its own.` })).toBeVisible({ timeout: 30_000 });
    await childInterrupted;
    await grandInterrupted;
    await leadWoken;
    await childWoken;
    for (const token of [started.childToken, started.grandToken]) {
      const row = ledgerRow(harness, token)!;
      expect(row.state, token).toBe('cancelled');
      expect(row.answer, token).toContain(USER_STOP);
      expect(row.notify, `${token} keeps its wake armed`).toBe(1);
    }
    if (provider === 'claude') {
      await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', started.child))
        .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(0);
    }
    await expect(child.getByTestId('agent-tray-row-state')).toHaveText('stopped');
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(0, { timeout: 30_000 });
    await expect(page.getByTestId('activity-rail-agents-toggle')).toHaveCount(0);
  });

  test(`${provider}: Stop All cascades; Background's Stop All and the thread's own Stop leave the rows`, async ({ harness, page }) => {
    test.setTimeout(300_000);
    const tree = await seedAgentsTree(harness, provider, `agents-all-${provider}`, { leadHolds: true });
    const started = await startTree(harness, tree);
    await awaitGate(harness, 'lead-hold', tree.leadPath);
    await harness.open(page);
    await awaitGate(harness, 'child-hold', tree.childPath);
    await openAgents(page, LEAD);
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(3);

    // Background's Stop All stops the lead's own shell and nothing listed
    // under Agents.
    await page.getByTestId('activity-rail-background-toggle').filter({ visible: true }).click();
    await page.getByTestId('activity-rail-background-stop-all').filter({ visible: true }).click();
    if (provider === 'claude') {
      await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', tree.lead))
        .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(0);
    }
    for (const token of [started.childToken, started.grandToken, started.sendToken]) {
      expect(ledgerRow(harness, token)!.state, token).toMatch(/running|accepted/);
    }
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(3);

    // The thread's own Stop ends its own turn and leaves what it started.
    const leadInterrupted = mockInterrupt(harness, tree.leadPath);
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    await leadInterrupted;
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    for (const token of [started.childToken, started.grandToken, started.sendToken]) {
      expect(ledgerRow(harness, token)!.state, token).toMatch(/running|accepted/);
    }
    await expect(agentRow(page, started.childToken).getByTestId('agent-tray-row-state')).toHaveText('running');

    // Stop All stops every row with the cascade, in one toast.
    const childInterrupted = mockInterrupt(harness, tree.childPath);
    const grandInterrupted = mockInterrupt(harness, tree.grandPath);
    await agentsBody(page).getByTestId('activity-rail-agents-stop-all').click();
    await expect(page.getByTestId('toast').filter({ hasText: 'Stopped 2 agents (2 turns, 1 background task and 1 agent of its own).' })).toBeVisible({ timeout: 30_000 });
    await childInterrupted;
    await grandInterrupted;
    for (const token of [started.childToken, started.grandToken, started.sendToken]) {
      const row = ledgerRow(harness, token)!;
      expect(row.state, token).toBe('cancelled');
      expect(row.answer, token).toContain(USER_STOP);
    }
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(0, { timeout: 30_000 });
  });
}
