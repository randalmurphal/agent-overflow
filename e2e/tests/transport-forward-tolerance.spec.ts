// Forward tolerance end to end (docs/specs/remote-access.md §9): a real
// backend pushing wire input a real SPA has never heard of, and the SPA
// carrying on.
//
// WHY THIS LEVEL. The unit suite
// (frontend/src/lib/transport/wsClient.test.ts, "runs normally against a
// future dialect") already drives every shape through the client in
// isolation, and it is the more thorough of the two: it can hand the
// client frame TYPES and frame-level FIELDS that no backend build emits.
// What only this level can prove is that the tolerance survives the real
// path — the event bus's channel registry, the fanout, the WebSocket, the
// SPA's own subscriber wiring — rather than only the parser. The swap
// window this guards (an already-loaded tab against a just-updated
// backend) is a browser holding a real page, not a unit fixture.
//
// WHAT THIS LEVEL CANNOT REACH. `HarnessEmit` publishes an EVENT on a
// caller-named channel, so the injection surface here is exactly one of
// the three §9 shapes at full fidelity — an unknown channel — plus
// unknown FIELDS, which ride inside the event payload. An unknown frame
// TYPE has no backend producer to borrow, and manufacturing one would
// mean a frame-injection hook in production code, which is a worse trade
// than leaving that shape to the unit suite. Stated rather than papered
// over: the header of a test should say what it does not prove.
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';

const THREAD_TITLE = 'Forward tolerance';
const ANSWER = 'A newer backend may add frames this build has never seen.';
const PATCHED_TITLE = 'Renamed by a future dialect';

interface ConsoleWatch {
  pageErrors: string[];
  errorLines: string[];
  /** Every console line, any level, for the "quiet, not merely alive" check. */
  allLines: string[];
}

/**
 * Arm the console listeners. Call AFTER the initial render has settled:
 * the verdict is about what the injection causes, and folding boot noise
 * from unrelated subsystems into it would make the spec fail for reasons
 * that have nothing to do with the wire.
 */
function watchConsole(page: Page): ConsoleWatch {
  const watch: ConsoleWatch = { pageErrors: [], errorLines: [], allLines: [] };
  page.on('pageerror', (err) => watch.pageErrors.push(String(err)));
  page.on('console', (msg) => {
    const line = `[${msg.type()}] ${msg.text()}`;
    watch.allLines.push(line);
    if (msg.type() === 'error') watch.errorLines.push(line);
  });
  return watch;
}

test('the SPA keeps running when the backend speaks a future dialect', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'forward-tolerance',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Tolerance\n' } }] },
        threads: [
          {
            title: THREAD_TITLE,
            turns: [
              {
                userText: 'What happens when the backend gets ahead of the bundle?',
                items: [{ kind: 'assistant_text', summary: ANSWER }],
              },
            ],
          },
        ],
      },
    ],
  });
  const threadId = seed.projects[0]!.threadIds[0]!;

  await page.goto(harness.url);
  await page.getByText(THREAD_TITLE).click();
  await expect(page.getByText(ANSWER)).toBeVisible();

  const watch = watchConsole(page);

  // Channels from a dialect that does not exist. Names, payload shapes,
  // and nesting depth all vary: the tolerance must be a property of the
  // dispatch path, not of one payload happening to be innocuous.
  await harness.rpc('HarnessEmit', 'lease:granted', {
    scopes: ['threads:read', 'threads:write'],
    ttlMs: 30_000,
  });
  await harness.rpc('HarnessEmit', 'device:paired', {
    deviceId: 'device-9',
    name: 'phone',
    pairedAt: { epochMs: 1_700_000_000_000, by: { principal: 'owner' } },
  });
  await harness.rpc('HarnessEmit', 'future:capability-grant', null);
  await harness.rpc('HarnessEmit', 'future:list', [1, 2, 3]);
  await harness.rpc('HarnessEmit', 'future:scalar', 'a bare string');

  // A channel this build DOES know, carrying fields it does not. The
  // known half must still apply: forward tolerance means ignoring the
  // additions, not discarding the frame that carried them.
  await harness.rpc('HarnessEmit', 'thread:updated', {
    action: 'patch',
    id: threadId,
    title: PATCHED_TITLE,
    originDeviceId: 'device-9',
    replicaEpoch: 4,
    grantedScopes: ['threads:write'],
  });

  // Both consumers of the patch, so a partial application cannot pass:
  // the sidebar row the event addresses by id, and the header of the
  // pane already showing that thread.
  await expect(page.getByTestId('chat-header-title')).toHaveText(PATCHED_TITLE);
  await expect(page.getByTestId('thread-row-title')).toHaveText(PATCHED_TITLE);

  // Still live afterwards, which is the actual claim — a page that
  // survived by wedging would also produce no errors. The viewport query
  // runs through the SPA's own store, so a corrupted one answers wrong
  // rather than not at all.
  const snapshot = await harness.rpc<{
    activeThreadId: string;
    panes: Array<{ threadId: string; rows: Array<{ textHead: string }> }>;
  }>('HarnessUIQuery', { v: 1, kind: 'viewport' });
  expect(snapshot.activeThreadId).toBe(threadId);
  const pane = snapshot.panes.find((candidate) => candidate.threadId === threadId);
  expect(pane, 'the seeded thread must still be mounted').toBeDefined();
  expect(pane!.rows.some((row) => row.textHead.includes('never seen'))).toBe(true);

  expect(watch.pageErrors).toEqual([]);
  expect(watch.errorLines).toEqual([]);

  // Quiet, not merely alive. A per-frame warn would turn a routine
  // version skew into console spam that buries whatever else went wrong,
  // so no injected channel may be named on any console line.
  const named = watch.allLines.filter((line) =>
    /lease:granted|device:paired|future:capability-grant|future:list|future:scalar/.test(line),
  );
  expect(named).toEqual([]);
});
