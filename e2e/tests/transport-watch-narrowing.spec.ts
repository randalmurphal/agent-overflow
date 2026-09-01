// Per-thread subscription narrowing end to end: the real SPA naming its
// open threads on a real socket, and the real backend answering with less.
//
// WHY THIS LEVEL. The unit suites already cover both halves in isolation —
// internal/transport/{event_entity,conn_watch}_test.go for the filter and
// the frame, frontend/src/lib/{transport,stores}/watchedThreads.test.ts for
// the client and the composition. What only this level can prove is that
// the two halves are WIRED to each other through the app that ships: that
// opening a pane is what produces the frame, that the set the backend holds
// is the set the panes imply, and that the ordering the design depends on
// survives the real click path rather than the fixture's.
//
// The socket is recorded in both directions by the shared recorder in
// transport-watch-helpers.ts; the badge-carrier sibling spec uses the same
// one on two pages at once.
import { test, expect, type SeedResult } from './fixtures.js';
import { readWire, recordWire, watchedNow } from './transport-watch-helpers.js';

const ANSWER_A = 'Answer belonging to the first thread.';
const ANSWER_B = 'Answer belonging to the second thread.';
const RETITLED = 'Retitled while nothing watched it';

test('the open panes are the threads the backend answers for', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'watch-narrowing',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Narrowing\n' } }] },
        threads: [
          {
            title: 'Thread alpha',
            turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: ANSWER_A }] }],
          },
          {
            title: 'Thread beta',
            turns: [{ userText: 'second', items: [{ kind: 'assistant_text', summary: ANSWER_B }] }],
          },
        ],
      },
    ],
  });
  const [alpha, beta] = seed.projects[0]!.threadIds;

  await recordWire(page);
  await harness.open(page);

  // Boot with nothing open states the empty set rather than staying
  // wildcard: a client with no panes has no consumer for these channels,
  // and "never sent one" is the different, wider state.
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([]);

  await page.getByText('Thread alpha').click();
  await expect(page.getByText(ANSWER_A)).toBeVisible();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([alpha]);

  await page.getByText('Thread beta').click();
  await expect(page.getByText(ANSWER_B)).toBeVisible();
  // The set is absolute, not additive: the pane replaced its thread, so
  // alpha is no longer watched by anything and drops out.
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([beta]);

  // ORDERING, the property the design turns on. The watch frame naming a
  // thread must precede every call that asks for that thread's content, or
  // the backend answers the history load while still withholding the
  // pushes that belong with it.
  const wire = await readWire(page);
  const watchIndex = wire.sent.findIndex(
    (frame) => frame.type === 'watch' && (frame.threads ?? []).includes(beta!),
  );
  const firstAsk = wire.sent.findIndex(
    (frame) => frame.type === 'rpc' && frame.text.includes(beta!),
  );
  expect(watchIndex, 'no watch frame ever named the opened thread').toBeGreaterThanOrEqual(0);
  expect(firstAsk, 'the opened thread was never asked about').toBeGreaterThanOrEqual(0);
  expect(watchIndex).toBeLessThan(firstAsk);
});

test('a narrowed channel is withheld for unwatched threads and nothing else is', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'watch-delivery',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Delivery\n' } }] },
        threads: [
          {
            title: 'Foreground thread',
            turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: ANSWER_A }] }],
          },
          {
            title: 'Background thread',
            turns: [{ userText: 'second', items: [{ kind: 'assistant_text', summary: ANSWER_B }] }],
          },
        ],
      },
    ],
  });
  const [watched, unwatched] = seed.projects[0]!.threadIds;

  await recordWire(page);
  await harness.open(page);
  await page.getByText('Foreground thread').click();
  await expect(page.getByText(ANSWER_A)).toBeVisible();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([watched]);

  const seedFor = (threadId: string) => ({
    threadId,
    itemId: `item-${threadId}`,
    path: 'README.md',
    language: 'markdown',
    files: [],
  });

  // Unwatched FIRST, watched second, on one channel and one connection:
  // frames are delivered in order, so the arrival of the second is proof
  // the first was dropped rather than merely slower.
  await harness.rpc('HarnessEmit', 'highlight:diff_seed', seedFor(unwatched!));
  await harness.rpc('HarnessEmit', 'highlight:diff_seed', seedFor(watched!));

  await expect
    .poll(async () => {
      const wire = await readWire(page);
      return wire.received
        .filter((event) => event.channel === 'highlight:diff_seed')
        .map((event) => event.threadId);
    })
    .toEqual([watched]);

  // The narrowing is per-channel, not per-connection. A thread with no
  // pane still drives every channel that was left wildcard — which is what
  // keeps the sidebar, the tray and the notification paths whole for work
  // the user is not currently looking at.
  await harness.rpc('HarnessEmit', 'thread:updated', {
    action: 'patch',
    id: unwatched,
    title: RETITLED,
  });
  await expect(page.getByText(RETITLED)).toBeVisible();
});
