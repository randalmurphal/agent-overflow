import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  emit,
  seedAgentThread,
  startMock,
  textLines,
} from './agent-visibility-helpers.js';

/**
 * The pipe under test is: `notifyOS` -> the transport sender -> the
 * `notification:send` channel a host-side presenter (the Windows
 * launcher) subscribes to -> a click -> `notification:activated` ->
 * frontend routing.
 *
 * An isolated boot installs the REAL sender, the same one `runHeadless`
 * does, so everything except the OS presentation itself is production
 * code. `HarnessNotify` therefore SUCCEEDS and the send is observable on
 * the wire; this spec used to assert a refusal stub's error message,
 * which meant the emission half was never executed at all.
 *
 * The second half of the file covers the EVENT MAPPING: a real turn on the
 * mock provider, the `emit` tap, the ordered dispatch queue, the SQLite
 * title read and the device-tier preference gate, all in a running app.
 * That is what the Go tests cannot prove — they exercise the tap with
 * hand-built payloads, not with the ones the triage router actually emits.
 */
type NotificationSend = {
  id: string;
  kind: string;
  retract?: boolean;
  title: string;
  body: string;
  target: { kind: string; threadId?: string; backendId?: string };
};

test('notification activation opens a seeded thread and none logs a no-op', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'notification-app',
        repo: {},
        threads: [
          {
            title: 'First notification thread',
            turns: [{ userText: 'first thread content', items: [] }],
          },
          {
            title: 'Target notification thread',
            turns: [{ userText: 'target thread content', items: [] }],
          },
        ],
      },
    ],
  });
  const [firstThreadId, targetThreadId] = seed.projects[0].threadIds;

  const activation = harness.waitForEvent<{ kind: string; threadId?: string }>(
    'notification:activated',
    (target) => target.kind === 'thread' && target.threadId === targetThreadId,
  );
  const sent = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (payload) => payload.target.kind === 'thread' && payload.target.threadId === targetThreadId,
  );

  await harness.rpc('HarnessNotify', 'Ready', 'Open the target thread', {
    kind: 'thread',
    threadId: targetThreadId,
  });

  // The send is what a presenter would have raised: a real id, the caller's
  // text verbatim, and the target that routes the click.
  const payload = await sent;
  expect(payload.id).toBeTruthy();
  expect(payload.title).toBe('Ready');
  expect(payload.body).toBe('Open the target thread');
  await activation;

  // The activation precedes the SPA connection to cover transport replay and
  // the bounded pre-hydration queue, not only the already-hydrated path.
  await harness.open(page);
  await expect(page.getByText('target thread content')).toBeVisible();
  await expect(page.getByText('first thread content')).not.toBeVisible();

  const noTargetLog = page.waitForEvent(
    'console',
    (message) => message.text() === 'notification:activated: no target',
  );
  const noTargetSend = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => data.target.kind === 'none',
  );
  await harness.rpc('HarnessNotify', 'Ready', 'Nothing to open', { kind: 'none' });
  // A targetless notification is still a real notification: it is raised
  // and only its CLICK is a no-op.
  expect((await noTargetSend).body).toBe('Nothing to open');
  expect((await noTargetLog).type()).toBe('info');

  expect(firstThreadId).not.toBe(targetThreadId);
});

/**
 * Four scripted turns on one thread. Each one answers with a line of text
 * and a result, so every send completes rather than leaving an open turn
 * behind at teardown.
 */
function fourTurnScenario() {
  return {
    version: 1,
    name: 'notification-turns',
    provider: 'claude',
    turns: [1, 2, 3, 4].map((n) => ({
      label: `turn-${n}`,
      steps: [emit([...textLines(`msg-${n}`, `Answer ${n}.`), RESULT_LINE])],
    })),
    afterTurns: 'silent',
  };
}

const turnComplete = (threadId: string) => (data: NotificationSend) =>
  !data.retract && data.kind === 'turn-complete' && data.target.threadId === threadId;

test('a completed turn notifies by name and resuming the thread withdraws it', async ({
  harness,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: fourTurnScenario() });
  const threadId = await seedAgentThread(harness, 'notify-mapping', 'Rewrite the parser');
  await startMock(harness, threadId);

  const completed = harness.waitForEvent<NotificationSend>('notification:send', turnComplete(threadId));
  await harness.rpc('SendMessage', threadId, 'first question', null);
  const presented = await completed;

  // Named after the thread, and saying only that it is at rest — the
  // assistant's own answer never crosses the redaction line.
  expect(presented.title).toBe('Rewrite the parser');
  expect(presented.body).toBe('Completed');
  expect(presented.body).not.toContain('Answer 1');
  expect(presented.id).toBe(`thread:${threadId}`);
  expect(presented.target.backendId).toBeTruthy();

  // Working again is the "handled elsewhere" that takes it back.
  const withdrawn = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => Boolean(data.retract) && data.id === presented.id,
  );
  const completedAgain = harness.waitForEvent<NotificationSend>(
    'notification:send',
    turnComplete(threadId),
  );
  await harness.rpc('SendMessage', threadId, 'second question', null);
  const retraction = await withdrawn;
  expect(retraction.title).toBe('');
  expect(retraction.target.kind).toBe('');

  // And the next rest re-presents under the SAME id, so one thread never
  // stacks two notifications.
  expect((await completedAgain).id).toBe(presented.id);
});

test('turning turn-complete off silences it without stranding a retraction', async ({
  harness,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: fourTurnScenario() });
  const threadId = await seedAgentThread(harness, 'notify-prefs', 'Preference thread');
  await startMock(harness, threadId);

  // The preference is device-tier and this connection names no device, so
  // the write lands on the backend machine's own screen — the same screen
  // the host-side sender reads. That is the pairing under test.
  await harness.rpc('UpdateSettings', { notifyTurnComplete: false });

  const before = harness.countEvents<NotificationSend>('notification:send', turnComplete(threadId));
  await harness.rpc('SendMessage', threadId, 'first question', null);
  await harness.waitForEvent('provider:turn_completed');

  // The barrier: the next turn's START queues a retraction BEHIND the
  // completion job on the same ordered queue, so observing it proves the
  // suppressed job has already run. Absence assertions need a barrier, not
  // a timeout.
  const withdrawn = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => Boolean(data.retract) && data.id === `thread:${threadId}`,
  );
  await harness.rpc('SendMessage', threadId, 'second question', null);
  await withdrawn;

  expect(harness.countEvents<NotificationSend>('notification:send', turnComplete(threadId)))
    .toBe(before);

  // A retraction is never gated: a toggle flipped mid-flight must not
  // strand what is already on a screen.
});

test('every notification send carries a distinct id', async ({ harness }) => {
  // Ids are what a presenter dedupes and replaces by, so two sends
  // colliding would silently collapse into one visible notification.
  const first = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => data.body === 'first',
  );
  const second = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => data.body === 'second',
  );
  await harness.rpc('HarnessNotify', 'Ready', 'first', { kind: 'none' });
  await harness.rpc('HarnessNotify', 'Ready', 'second', { kind: 'none' });

  const [a, b] = await Promise.all([first, second]);
  expect(a.id).toBeTruthy();
  expect(b.id).toBeTruthy();
  expect(a.id).not.toBe(b.id);
});
