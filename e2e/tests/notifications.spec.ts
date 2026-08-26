import { test, expect, type SeedResult } from './fixtures.js';

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
 */
type NotificationSend = {
  id: string;
  title: string;
  body: string;
  target: { kind: string; threadId?: string };
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
  await page.goto(harness.url);
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
