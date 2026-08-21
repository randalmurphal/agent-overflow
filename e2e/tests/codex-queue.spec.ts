// Codex's PROVIDER-OWNED message queue (`thread/queue/*`, app-server >=
// 0.148), end to end against the real backend and the real mock binary.
//
// The contract under test, and the reason it needs this level: on a
// connected 0.148+ app-server AO stops steering a mid-turn message into the
// running turn and hands it to the provider's queue instead
// (app_flush_queue.go `usesProviderQueue`). The app-server then dispatches it
// ITSELF when the thread goes idle — no `turn/start` and no
// `thread/queue/start` from us. So the failure this spec exists to catch is
// double delivery: AO's own flush queue dispatching the same message the
// provider already holds. Unit tests can prove each half; only a run where
// the mock owns the dispatch clock can prove the two queues are mutually
// exclusive.
//
// The mock's `initialize` reports `codex_cli_rs/99.0.0`, which is what opens
// the version gate — the same handshake field a real app-server answers with.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

interface Item {
  kind: string;
  status: string;
  summary: string;
}

test('messages sent during a codex turn go to the provider queue and dispatch once, in order', async ({
  harness,
}) => {
  await harness.rpc('HarnessSetScenario', { name: 'codex-queue-while-running' });

  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'codex-queue',
        repo: {},
        threads: [{ title: 'Provider queue', provider: 'codex' }],
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

  // Two more messages while that turn is held. Each is dispatched
  // immediately by AO's flush queue — but on a provider-queue session
  // "dispatch" means `thread/queue/add`, which starts nothing.
  await harness.rpc('RegisterQueueItem', threadId, 'second prompt', {});
  await harness.rpc('RegisterQueueItem', threadId, 'third prompt', {});

  // Barrier: both messages have left AO for the provider's queue. The row is
  // persisted and `provider:queue_flushed` emitted at the same point the add
  // succeeds, so once two have arrived, anything AO was going to do with
  // these messages it has already done.
  await harness.waitForEvent<{ threadId: string }>(
    'provider:queue_flushed',
    (ev) => ev.threadId === threadId,
  );
  await harness.waitForEvent<{ threadId: string }>(
    'provider:queue_flushed',
    (ev) => ev.threadId === threadId,
  );

  // Neither has reached the model: the mock reports user_input only when a
  // turn actually carries the text, and the queued pair cannot until the
  // held turn ends. A second `user_input` here would mean AO steered or
  // started a turn behind the provider's back. Counted rather than waited
  // on, because waitForEvent replays the log and so cannot prove an absence.
  expect(
    harness.countEvents<HarnessMockEvent>(
      'harness:mock',
      (ev) => ev.report.kind === 'user_input',
    ),
  ).toBe(1);

  // All three rows ARE persisted already, and that is the point: on a
  // provider-queue session the user row is the ownership record. Once
  // `thread/queue/add` returns, the message is durable in codex's SQLite and
  // runs on the next resume whether or not this process survives, so an
  // in-memory marker could not describe it (app_flush_queue.go, the
  // `providerQueued` persist branch). The rows are written QUIETLY — no
  // provider:item_event — so the UI still shows them as queued markers above
  // the composer, not as transcript messages, exactly as Claude's own
  // eager-persisted queued sends do.
  const midTurnItems = await harness.rpc<Item[]>('ListItems', threadId);
  expect(midTurnItems.filter((i) => i.kind === 'user_text').map((i) => i.summary)).toEqual([
    'first prompt',
    'second prompt',
    'third prompt',
  ]);

  // Release turn 1. From here the mock's idle hook owns the clock.
  await advance('hold-first-turn');
  await harness.waitForEvent('provider:turn_completed');

  // One dispatch per queued message, in the order they were queued.
  expect((await nextUserInput()).report.input).toBe('second prompt');
  await harness.waitForEvent('provider:turn_completed');
  expect((await nextUserInput()).report.input).toBe('third prompt');
  await harness.waitForEvent('provider:turn_completed');

  // The transcript is the real assertion: three prompts, three answers, each
  // prompt exactly once. A duplicate here is the double-delivery bug.
  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      return items
        .filter((i) => i.kind === 'user_text' || i.kind === 'assistant_text')
        .map((i) => i.summary);
    })
    .toEqual([
      'first prompt',
      'Finished turn 1.',
      'second prompt',
      'Answering: second prompt',
      'third prompt',
      'Answering: third prompt',
    ]);

  // Every queued message ran as its OWN turn (the provider queue never
  // steers into a running one), so each prompt sits in a distinct turn.
  const settled = await harness.rpc<Item[]>('ListItems', threadId);
  expect(settled.filter((i) => i.status !== 'completed')).toEqual([]);
});
