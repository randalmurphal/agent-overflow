import { test, expect, type SeedResult } from './fixtures.js';

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
  await expect(
    harness.rpc('HarnessNotify', 'Ready', 'Open the target thread', {
      kind: 'thread',
      threadId: targetThreadId,
    }),
  ).rejects.toThrow(/method_error: OS notifications are unavailable/);
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
  await expect(
    harness.rpc('HarnessNotify', 'Ready', 'Nothing to open', { kind: 'none' }),
  ).rejects.toThrow(/method_error: OS notifications are unavailable/);
  expect((await noTargetLog).type()).toBe('info');

  expect(firstThreadId).not.toBe(targetThreadId);
});
