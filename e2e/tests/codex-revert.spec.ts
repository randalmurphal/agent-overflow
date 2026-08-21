// Edit-and-resend on a Codex thread, and which of upstream's two history
// cuts the app chooses.
//
// `thread/revert` (app-server >= 0.148) truncates the thread IN PLACE:
// same provider thread id, so SessionRef, provider-side thread cost and
// any external `codex resume` of it all keep pointing at the thread the
// user is editing. `thread/fork` answers the same question with a NEW
// thread and AO repoints SessionRef at it. The choice is gated twice —
// the app-server's handshake version AND the thread's own history mode,
// which is decided once at `thread/start` and is legacy by default — and
// that is exactly the kind of thing unit tests cannot settle: the cut
// runs through a THROWAWAY RESUME session (the rollback stops the live
// one first), a second provider process that never saw the start. Only a
// run where the mock owns its own thread store can prove the resumed
// connection still knows the thread is paginated.
//
// Both legs assert the same two things from opposite sides: which method
// the provider was asked for, and whether the thread's identity survived.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

interface Item {
  id: string;
  kind: string;
  status: string;
  summary: string;
}

interface Thread {
  id: string;
  sessionRef?: string;
}

/** Runs two answered turns on a fresh codex thread of the given scenario. */
async function twoTurnCodexThread(
  harness: {
    rpc: <T>(method: string, ...args: unknown[]) => Promise<T>;
    waitForEvent: <T>(channel: string, predicate?: (data: T) => boolean) => Promise<T>;
  },
  scenario: string,
): Promise<string> {
  await harness.rpc('HarnessSetScenario', { name: scenario });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: scenario,
        repo: {},
        threads: [{ title: 'Editable', provider: 'codex' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  for (const text of ['first question', 'second question']) {
    await harness.rpc('SendMessage', threadId, text, null);
    await harness.waitForEvent('provider:turn_completed');
  }
  return threadId;
}

async function userItemId(
  harness: { rpc: <T>(method: string, ...args: unknown[]) => Promise<T> },
  threadId: string,
  summary: string,
): Promise<string> {
  const items = await harness.rpc<Item[]>('ListItems', threadId);
  const match = items.find((i) => i.kind === 'user_text' && i.summary === summary);
  if (!match) throw new Error(`no user_text ${JSON.stringify(summary)} in ${JSON.stringify(items)}`);
  return match.id;
}

test('editing a message on a paginated codex thread cuts it in place', async ({ harness }) => {
  const threadId = await twoTurnCodexThread(harness, 'codex-revert-paginated');
  const before = await harness.rpc<Thread>('GetThread', threadId);
  expect(before.sessionRef).toBe('mock-codex-revert-paginated');

  const target = await userItemId(harness, threadId, 'second question');
  const started = Date.now();
  await harness.rpc('RevertConversationAndResendMessage', threadId, target, {
    content: 'second question, rephrased',
  });
  const elapsed = Date.now() - started;

  // Which cut the provider was actually asked for. The anchor is the
  // FIRST DROPPED turn (revert's exclusive boundary), not the last kept
  // one the fork would name.
  const cut = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'history_cut',
  );
  expect(cut.report.detail).toBe('thread/revert');
  expect(cut.report.input).toBe('turn-2');

  // The identity that survives the cut is the whole point of preferring
  // it: a fork would have answered with a brand new thread id here.
  const after = await harness.rpc<Thread>('GetThread', threadId);
  expect(after.sessionRef).toBe(before.sessionRef);

  // The `thread/reverted` echo, end to end. Revert waits up to 5s for it
  // and only then gives up, so a saga that returned in a fraction of that
  // is one whose armed expectation actually matched the notification.
  expect(elapsed).toBeLessThan(4_500);

  // The edited message replaced the old tail rather than appending to it.
  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      return items
        .filter((i) => i.kind === 'user_text' || i.kind === 'assistant_text')
        .map((i) => i.summary);
    })
    .toEqual([
      'first question',
      'Answering: first question',
      'second question, rephrased',
      'Answering: second question, rephrased',
    ]);
});

test('editing a message falls back to a fork when the app-server predates thread/revert', async ({
  harness,
}) => {
  const threadId = await twoTurnCodexThread(harness, 'codex-revert-legacy');
  const before = await harness.rpc<Thread>('GetThread', threadId);
  expect(before.sessionRef).toBe('mock-codex-revert-legacy');

  const target = await userItemId(harness, threadId, 'second question');
  await harness.rpc('RevertConversationAndResendMessage', threadId, target, {
    content: 'second question, rephrased',
  });

  // No revert was even attempted: the 0.147 handshake closes the version
  // gate before any RPC, and the thread it started is legacy anyway. The
  // fork's anchor is the LAST KEPT turn, one turn below the revert's.
  const cut = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'history_cut',
  );
  expect(cut.report.detail).toBe('thread/fork');
  expect(cut.report.input).toBe('turn-1');

  // The cost of the fallback, made visible: the thread the user is
  // editing now points at a different Codex thread.
  const after = await harness.rpc<Thread>('GetThread', threadId);
  expect(after.sessionRef).toBe('mock-codex-revert-legacy-fork-1');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      return items
        .filter((i) => i.kind === 'user_text' || i.kind === 'assistant_text')
        .map((i) => i.summary);
    })
    .toEqual([
      'first question',
      'Answering: first question',
      'second question, rephrased',
      'Answering: second question, rephrased',
    ]);
});
