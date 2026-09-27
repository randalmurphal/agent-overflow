// A streaming reasoning row's reveal work is bounded by the delta, not by the
// text revealed so far: the row's summary is the trim of its previous summary
// plus the delta, and the collapsed clamp renders a bounded window
// (utils/liveText.ts). This counts the characters each summary trim reads
// while a long row streams.
import { expect, it, vi } from 'vitest';

const trims = vi.hoisted(() => ({ inputLengths: [] as number[] }));

vi.mock('./threadPaneShared', async (importOriginal) => {
  const original = await importOriginal<typeof import('./threadPaneShared')>();
  return {
    ...original,
    trimToTailRunes: (text: string, maxRunes: number): string => {
      trims.inputLengths.push(text.length);
      return original.trimToTailRunes(text, maxRunes);
    },
  };
});

it('trims a streaming reasoning summary from its previous summary and the delta', async () => {
  // The test setup loads the pane modules before this file's mock applies;
  // a fresh module graph binds them to the recording trim. It repeats the
  // setup's connection and grant pins for that graph.
  vi.resetModules();
  (await import('./transportStatus.svelte')).__setTransportStatusForTest({
    status: 'connected',
    nextAttemptAt: null,
  });
  (await import('../transport/scopes')).setPageGrantsFromBootstrap(false);
  const { buildPane, makeItem, makeThread } = await import('../../test/helpers/chat');
  const { FakeSmoothingClock, installThreadPaneTestEnv } = await import(
    '../../test/helpers/threadPane'
  );
  const { getSettings } = await import('./settings.svelte');
  const { __setSmoothingClockForTest, trimToTailRunes } = await import('./threadPaneShared');
  installThreadPaneTestEnv();
  const clock = new FakeSmoothingClock();
  __setSmoothingClockForTest(clock);
  // Low power reveals each wire chunk whole, so a reveal delta is a chunk.
  getSettings().lowPowerMode = true;
  const pane = await buildPane(makeThread({ id: 'thread-work' }));
  const id = 'think:0:0';
  pane.upsertItem(
    makeItem({
      id,
      threadId: 'thread-work',
      kind: 'thinking',
      role: 'assistant',
      status: 'streaming',
      summary: '',
      payloadId: `thinking:${id}`,
      updatedAt: 1,
    }),
  );
  const text = Array.from(
    { length: 2_000 },
    (_, line) => `🧠 step ${line} weighs the next option\n`,
  ).join('');
  const maxChunk = 120;
  trims.inputLengths.length = 0;
  for (let sent = 0, chunk = 0; sent < text.length; chunk++) {
    const next = Math.min(text.length, sent + 1 + ((chunk * 7919) % maxChunk));
    pane.applyItemDelta({
      threadId: 'thread-work',
      itemId: id,
      kind: 'thinking',
      delta: text.slice(sent, next),
      updatedAt: 2 + chunk,
    });
    sent = next;
    clock.tickFrame(16);
  }
  const inputLengths = trims.inputLengths.splice(0);
  expect(inputLengths.length).toBeGreaterThan(text.length / maxChunk);
  // A 400-rune summary is at most 800 code units.
  expect(Math.max(...inputLengths)).toBeLessThanOrEqual(800 + maxChunk);
  expect(pane.items[0].summary).toBe(trimToTailRunes(text, 400));
});
