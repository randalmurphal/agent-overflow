// Codex mid-turn dispatch, end to end against the real backend and the real
// mock binary.
//
// The contract under test: a message sent while a Codex turn is running goes
// out as `turn/steer`, into the RUNNING turn, stamped with the AO row id as
// `clientUserMessageId`. It never goes to the app-server's own
// `thread/queue/*` — that queue dispatches on ITS clock, so a message handed
// over would have two dispatchers. The mock refuses `thread/queue/add` and
// `thread/queue/start` outright, which is what makes a regrown queue caller a
// failing run rather than a duplicated turn.
//
// The other half is identity. Every codex send registers its pending entry BY
// the client id it stamped, and an entry waiting to be named is invisible to
// an id-less echo — so a steer whose echo carried no `clientId` would leave
// the message rendering as "Injected provider context" instead of the user's
// own row. Unit tests can prove each half; only a run where the mock owns the
// wire can prove the two agree.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

interface Item {
  kind: string;
  status: string;
  summary: string;
}

test('messages sent during a codex turn steer into it and land in order', async ({ harness }) => {
  await harness.rpc('HarnessSetScenario', { name: 'codex-steer-while-running' });

  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'codex-steer',
        repo: {},
        threads: [{ title: 'Mid-turn steer', provider: 'codex' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  const mockId = registered.mockId;
  const advance = (name: string) =>
    harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name });
  const gate = (name: string) =>
    harness.waitForEvent<HarnessMockEvent>(
      'harness:mock',
      (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === name,
    );
  const nextUserInput = () =>
    harness.waitForEvent<HarnessMockEvent>(
      'harness:mock',
      (ev) => ev.report.kind === 'user_input',
    );

  // Turn 1 opens and parks with a command still running.
  await harness.rpc('SendMessage', threadId, 'first prompt', null);
  expect((await nextUserInput()).report.input).toBe('first prompt');
  await gate('hold-first-turn');

  // Two more messages while that turn is held. AO's flush queue dispatches
  // each one immediately — as a steer into the turn that is already running.
  await harness.rpc('RegisterQueueItem', threadId, 'second prompt', {});
  await harness.rpc('RegisterQueueItem', threadId, 'third prompt', {});

  // The mock reports steer input on the same surface as a turn's own input,
  // so both messages show up here — in the order they were queued, and
  // WITHOUT a new turn behind either of them.
  expect((await nextUserInput()).report.input).toBe('second prompt');
  expect((await nextUserInput()).report.input).toBe('third prompt');

  // Still exactly one turn: a steer joins the running turn's input rather
  // than starting anything. A second `turn/started` here would mean AO fell
  // back to a fresh turn — or that the provider queue dispatched behind its
  // back.
  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId, true);
      return items.filter((i) => i.kind === 'user_text').map((i) => i.summary);
    })
    .toEqual(['first prompt', 'second prompt', 'third prompt']);

  // Every steered row resolved its pending send: the mock echoed the
  // `clientUserMessageId` back as `clientId`, so each message landed on the
  // row it was minted for. An unmatched echo would have persisted as a
  // notification ("Injected provider context") instead.
  const midTurnItems = await harness.rpc<Item[]>('ListItems', threadId, true);
  expect(midTurnItems.filter((i) => i.kind === 'notification')).toEqual([]);

  // Release turn 1 and let it finish.
  await advance('hold-first-turn');
  await harness.waitForEvent('provider:turn_completed');

  // The transcript is the real assertion: three prompts inside one turn,
  // each exactly once, then the turn's answer. A duplicate here is the
  // double-delivery bug the queue revert exists to prevent.
  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId, true);
      return items
        .filter((i) => i.kind === 'user_text' || i.kind === 'assistant_text')
        .map((i) => i.summary);
    })
    .toEqual(['first prompt', 'second prompt', 'third prompt', 'Finished turn 1.']);

  const settled = await harness.rpc<Item[]>('ListItems', threadId, true);
  expect(settled.filter((i) => i.status !== 'completed')).toEqual([]);
});

// Hold only the isolated mock. A steer can be written while the provider
// cannot answer, exposing the real queue -> worker -> live-snapshot seam.
test('repeated Codex sends remain pending through reload until provider consumption', async ({ harness, page }) => {
  let holdAdmission = false;
  let releaseSubmission: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (holdAdmission && frame.type === 'rpc' && frame.methodId === 1034543696) {
        holdAdmission = false;
        releaseSubmission = () => server.send(message);
      } else server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  await harness.rpc('HarnessSetScenario', { name: 'codex-steer-while-running' });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'codex-pending', repo: {}, threads: [{ title: 'Pending Codex', provider: 'codex', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }] }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.open(page);
  await page.getByText('Pending Codex', { exact: true }).click();
  const registered = harness.waitForEvent<HarnessMockEvent>('harness:mock', ev => ev.report.kind === 'registered');
  await harness.rpc('StartSession', threadId);
  const mockId = (await registered).mockId;
  const held = harness.waitForEvent<HarnessMockEvent>('harness:mock', ev => ev.mockId === mockId && ev.report.kind === 'waiting_signal' && ev.report.detail === 'hold-first-turn');
  await page.getByLabel('Message Input').fill('first prompt');
  await page.getByLabel('Message Input').press('Enter');
  await held;
  const mocks = await harness.rpc<Array<{ mockId: string; registration: { pid: number } }>>('HarnessListMocks');
  const pid = mocks.find(mock => mock.mockId === mockId)?.registration.pid;
  expect(pid).toBeGreaterThan(0);
  process.kill(pid!, 'SIGSTOP');
  const messages = ['pending Codex message one', 'pending Codex message two'];
  try {
    holdAdmission = true;
    for (const message of messages) {
      const accepted = harness.waitForEvent<any>('provider:queue_state_changed', ev => ev.threadId === threadId && ev.items.some((item: any) => item.message === message));
      await page.getByLabel('Message Input').fill(message);
      await page.getByLabel('Message Input').press('Enter');
      if (message === messages[0]) {
        await expect.poll(() => !!releaseSubmission).toBe(true);
        // The RPC has not reached the host yet. The cleared composer must
        // already have handed the text to a provisional pending preview.
        await expect(page.getByLabel('Message Input')).toHaveValue('');
        await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: message })).toBeVisible();
        releaseSubmission!();
      }
      await accepted;
      await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: message })).toBeVisible();
      await expect(page.getByTestId('user-message-bubble').filter({ hasText: message })).toHaveCount(0);
    }
    await page.reload();
    for (const message of messages) {
      await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: message })).toBeVisible();
      await expect(page.getByTestId('user-message-bubble').filter({ hasText: message })).toHaveCount(0);
    }
  } finally {
    process.kill(pid!, 'SIGCONT');
  }
  for (const message of messages) {
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: message })).toBeVisible();
    await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: message })).toHaveCount(0);
  }
  const finished = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name: 'hold-first-turn' });
  await finished;
  await page.reload();
  for (const message of messages) await expect(page.getByText(message, { exact: true })).toHaveCount(1);
});
