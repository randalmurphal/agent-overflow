// The activity rail's Agents segment (docs/specs/standing-agents.md,
// "Agents segment"), end to end on both mock providers.
//
// Coverage: a spawn appears as a running row with its title and provider; a
// send with notify appears; the spawn's own spawn nests under it; a row
// blocked on an approval says so, and runs again once it is answered; a
// settled row shows its outcome, and its click lands on the wake item that
// carried its answer; a click opens the target in a pane; a row's Stop
// cancels the request for the user, keeps the caller's wake armed with the
// user named, interrupts the spawned child, stops the child's background
// shell and cascades into the grandchild, whose request is cancelled with
// its wake disarmed, so the child takes no new turn; the stopped row
// lingers and leaves; Stop All cascades;
// Background's Stop All leaves the rows; the thread's own Stop does not
// cascade; and an answer held in the caller's queue (its session cannot
// start) is an incoming row there, named by its sender and kind, whose Stop
// withdraws it and leaves the answer owed with its wake disarmed. The
// cross-computer rows are thread-tools-paired.spec.ts's.
import { test, expect, type SeedResult } from './fixtures.js';
import { rename } from 'node:fs/promises';
import { advanceGate, awaitGate, awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { scrollTimelineToTop } from './standing-agent-home-contents-helpers.js';
import {
  CHILD,
  GRAND,
  LEAD,
  PEER,
  agentRow,
  approvalTool,
  agentsBody,
  heldSendIds,
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

    // The peer answers: its row shows the outcome, and once the lead took
    // the answer in, its click leaves the tray and lands on the wake item.
    const peerHold = await awaitGate(harness, 'peer-hold', tree.peerPath);
    await advanceGate(harness, peerHold.mockId, 'peer-hold');
    await expect(send).toHaveAttribute('data-settled', 'true', { timeout: 30_000 });
    await expect(send.getByTestId('agent-tray-row-state')).toHaveText(/finished|replied/);
    await expect(send.getByTestId('agent-tray-row-provider')).toHaveAttribute('data-provider', provider);
    await capture(page, `agents-${provider}-settled.png`, agentsBody(page));
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveText('2');
    const sendOpen = send.getByTestId('agent-tray-row-open');
    await expect(sendOpen).toHaveAttribute('aria-label', `Show the answer from ${PEER}`, { timeout: 30_000 });
    const listing = await harness.rpc<{ rows: { token: string; wakeItemId?: string }[] }>('ListThreadAgentRows', tree.lead);
    const wakeItemId = listing.rows.find((row) => row.token === started.sendToken)?.wakeItemId;
    expect(wakeItemId, 'the settled send names its wake item').toBeTruthy();
    // Scrolled up to the lead's first message, past a report longer than
    // the pane, the wake row is out of view; only the click can bring it in.
    const timeline = page.getByTestId('message-timeline-scroll').filter({ visible: true }).first();
    const wakeItem = timeline.locator(`[data-item-id="${wakeItemId}"]`);
    const firstAsk = timeline.getByText('run the release', { exact: true });
    await scrollTimelineToTop(page, firstAsk);
    await expect(firstAsk).toBeInViewport();
    if (await wakeItem.count() > 0) await expect(wakeItem).not.toBeInViewport();
    await sendOpen.click();
    await expect(wakeItem).toBeInViewport({ timeout: 10_000 });
    await expect(send).toHaveCount(0);

    // A click opens the child in a pane.
    await child.getByTestId('agent-tray-row-open').click();
    await expect(page.getByTestId('chat-header-title').filter({ hasText: CHILD })).toBeVisible();
    await page.getByTestId('thread-row-title').getByText(LEAD, { exact: true }).click();
    await expect(agentsBody(page)).toBeVisible();

    // The child's background shell is listed in its own Backgrounded set.
    await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', started.child))
      .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(1);

    // Stop on the child's row: the request is cancelled for the user and
    // the lead woken with the user named; the child's turn is interrupted,
    // its shell stopped, and the grandchild's request cancelled with its
    // wake disarmed and its turn interrupted, so the child, whose work the
    // person stopped, takes no new turn for it.
    const childInterrupted = mockInterrupt(harness, tree.childPath);
    const grandInterrupted = mockInterrupt(harness, tree.grandPath);
    const leadWoken = mockInput(harness, tree.leadPath, [started.childToken, USER_STOP]);
    const childWoken = mockInput(harness, tree.childPath, [started.grandToken], 20_000).then(() => true, () => false);
    await child.getByTestId('agent-tray-row-stop').click();
    await expect(page.getByTestId('toast-message').filter({ hasText: `Stopped ${CHILD}` }))
      .toHaveText(`Stopped ${CHILD}: its turn, 1 background task and 1 agent of its own.`, { timeout: 30_000 });
    await childInterrupted;
    await grandInterrupted;
    await leadWoken;
    const lead = ledgerRow(harness, started.childToken)!;
    expect(lead.state).toBe('cancelled');
    expect(lead.answer).toContain(USER_STOP);
    expect(lead.notify, 'the pressed row keeps its wake armed').toBe(1);
    const nested = ledgerRow(harness, started.grandToken)!;
    expect(nested.state).toBe('cancelled');
    expect(nested.answer).toContain(USER_STOP);
    expect(nested.notify, 'the cascade disarms the nested wake').toBe(0);
    await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', started.child))
      .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(0);
    // The stopped row lingers, then leaves.
    await expect(child.getByTestId('agent-tray-row-state')).toHaveText('stopped');
    const settledAt = Date.now();
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(0, { timeout: 30_000 });
    expect(Date.now() - settledAt, 'the settled row lingered').toBeGreaterThan(5_000);
    await expect(page.getByTestId('activity-rail-agents-toggle')).toHaveCount(0);
    expect(await childWoken, 'the child took a turn for the agent the person stopped').toBe(false);
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
    await expect.poll(async () => (await harness.rpc<{ status: string }[]>('ListLiveBackgroundTasks', tree.lead))
      .filter((item) => item.status === 'running').length, { timeout: 30_000 }).toBe(0);
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
    await expect(page.getByTestId('toast-message').filter({ hasText: 'Stopped 2 agents' }))
      .toHaveText('Stopped 2 agents (2 turns, 1 background task and 1 agent of its own).', { timeout: 30_000 });
    await childInterrupted;
    await grandInterrupted;
    for (const token of [started.childToken, started.grandToken, started.sendToken]) {
      const row = ledgerRow(harness, token)!;
      expect(row.state, token).toBe('cancelled');
      expect(row.answer, token).toContain(USER_STOP);
      // The lead's own rows tell it; the child is not woken for its spawn.
      expect(row.notify, token).toBe(token === started.grandToken ? 0 : 1);
    }
    await expect(agentsBody(page).getByTestId('agent-tray-row')).toHaveCount(0, { timeout: 30_000 });
  });

  test(`${provider}: an answer held in the caller's queue is an incoming row there, and its Stop withdraws it`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `incoming-${provider}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
        { name: `incoming-${provider}-peer`, repo: {}, threads: [{ title: PEER, provider }] },
      ],
    });
    const [leadProject, peerProject] = seed.projects;
    const lead = leadProject.threadIds[0];
    const peer = peerProject.threadIds[0];
    await setScenario(harness, leadProject.path, threadToolsScenario({
      name: `incoming-${provider}-lead`, provider, echoUserMessage: true,
      turns: [{ steps: [{ call: { tool: 'thread_send', args: { thread_id: peer, message: 'Check the changelog.', notify: true } } }], text: 'Asked the peer.' }],
    }));
    await setScenario(harness, peerProject.path, threadToolsScenario({
      name: `incoming-${provider}-peer`, provider, echoUserMessage: true,
      turns: [{ steps: [{ gate: 'peer-hold' }], text: 'The changelog is fine.' }],
    }));
    await harness.rpc('StartSession', lead);
    await harness.rpc('SendMessage', lead, 'ask the peer', null);
    const send = await awaitToolAnswer<{ token: string }>(harness, { tool: 'thread_send', cwd: leadProject.path, timeoutMs: 60_000 });
    expect(send.isError, send.text).toBe(false);
    const token = send.value!.token;
    await awaitTurnCompleted(harness, lead);
    const peerHold = await awaitGate(harness, 'peer-hold', peerProject.path);

    // A live session takes a queued message at once; the caller's session
    // has ended and cannot start again, so the answer waits in its queue.
    await harness.rpc('StopSession', lead);
    const away = `${leadProject.path}.away`;
    if (provider === 'codex') {
      await setScenario(harness, leadProject.path, {
        ...threadToolsScenario({ name: `incoming-${provider}-refused`, provider, turns: [{ text: 'unreached' }] }),
        codex: { responses: {
          initialize: '{"jsonrpc":"2.0","id":${REQUEST_ID},"error":{"code":-32603,"message":"mock: the app-server refused to start"}}',
        } },
      });
    } else {
      await rename(leadProject.path, away);
    }
    try {
      await advanceGate(harness, peerHold.mockId, 'peer-hold');
      await expect.poll(() => heldSendIds(harness, lead), { timeout: 30_000 }).toEqual([`thread-wake:${token}`]);

      await harness.open(page);
      await openAgents(page, LEAD);
      const incoming = agentsBody(page).getByTestId('agent-incoming-row');
      await expect(incoming).toHaveCount(1);
      await expect(incoming).toHaveAttribute('data-kind', 'reply');
      await expect(incoming.getByTestId('agent-incoming-row-sender')).toHaveText(`from ${PEER}`);
      await capture(page, `agents-${provider}-incoming.png`, agentsBody(page));

      await incoming.getByTestId('agent-incoming-row-stop').click();
      await expect(page.getByTestId('toast').filter({ hasText: "Withdrew the incoming reply before this thread's agent read it." })).toBeVisible({ timeout: 30_000 });
      expect(heldSendIds(harness, lead)).toEqual([]);
      // The answer stays owed, for thread_status to read, and its wake is disarmed.
      const row = ledgerRow(harness, token)!;
      expect(row.state).toMatch(/finished|replied/);
      expect(row.notify).toBe(0);
      await expect(incoming).toHaveCount(0);
    } finally {
      if (provider === 'claude') await rename(away, leadProject.path).catch(() => undefined);
    }
  });
}
