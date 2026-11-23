// The upgrade corpus top-up's at-rest check (assertAtRest in
// upgrade-corpus-topup.ts), which every top-up step runs when it returns,
// and the coverage gate's naming of the rows a stale allowlist entry is on
// (upgrade-corpus-coverage.ts). A turn held at a gate after its streaming
// row is written, and a sent message whose turn has not opened because
// the backend holds the provider's frames, each fail the check naming their thread, and pass it once they end. An
// entry the store writes after all fails the gate naming the rows that
// carry it, a whole-table entry included.
import { randomBytes } from 'node:crypto';
import * as path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { awaitTurnCompleted, plainScenario, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { assertCoverage, measureCoverage, readSchema } from './upgrade-corpus-coverage.js';
import { assertAtRest, storeThreads } from './upgrade-corpus-topup.js';

const TURN_TIMEOUT_MS = 30_000;

async function seedThread(harness: HarnessApp, title: string): Promise<{ projectId: string; cwd: string; threadId: string }> {
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `at-rest-${randomBytes(3).toString('hex')}`, repo: {}, threads: [{ title, provider: 'claude' }] }],
  });
  const [{ projectId, path: cwd, threadIds: [threadId] }] = seeded.projects;
  return { projectId, cwd, threadId };
}

function streamingRows(harness: HarnessApp, threadId: string): number {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return (database.prepare(`SELECT count(*) AS n FROM items WHERE thread_id = ? AND status = 'streaming'`).get(threadId) as { n: number }).n;
  } finally {
    database.close();
  }
}

test('the at-rest check names a thread whose turn is held mid-stream, and passes once the turn ends', async ({ harness }) => {
  const inherited = storeThreads(harness);
  const title = 'At rest: held mid-stream';
  const { projectId, cwd, threadId } = await seedThread(harness, title);
  const gate = `at-rest-held-${randomBytes(3).toString('hex')}`;
  await setScenario(harness, cwd, threadToolsScenario({
    name: gate,
    provider: 'claude',
    turns: [{
      steps: [
        {
          emitLines: [
            '{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-${TURN}","role":"assistant"}}}',
            '{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}',
            '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Halfway"}}}',
          ],
        },
        { gate },
        {
          emitLines: [
            '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":" there."}}}',
            '{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}',
            '{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}',
            '{"type":"assistant","message":{"id":"msg-${TURN}","role":"assistant","content":[{"type":"text","text":"Halfway there."}]}}',
          ],
        },
      ],
      text: '',
    }],
  }));
  await harness.rpc('StartSession', threadId);
  const held = harness.waitForEvent<{ mockId: string; cwd: string; report: { kind: string; detail?: string } }>(
    'harness:mock', (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === gate && ev.cwd === cwd, TURN_TIMEOUT_MS);
  await harness.rpc('SendMessage', threadId, 'Start and stop halfway.', null);
  const parked = await held;
  await expect.poll(() => streamingRows(harness, threadId), {
    message: 'the held turn never wrote its streaming row',
    timeout: TURN_TIMEOUT_MS,
  }).toBeGreaterThan(0);

  await expect(assertAtRest(harness, 'held', inherited)).rejects.toThrow(
    new RegExp(`top-up step held returned while 1 thread\\(s\\) still run or owe a turn; .*\\n  - "${title}" \\(${threadId}\\): a turn is open`, 's'));

  const ended = awaitTurnCompleted(harness, threadId, TURN_TIMEOUT_MS);
  await harness.rpc('HarnessMockCommand', parked.mockId, { type: 'advance', name: gate });
  await ended;
  await assertAtRest(harness, 'released', inherited);
  expect(streamingRows(harness, threadId)).toBe(0);

  // The gate names the rows an entry the store writes after all is on,
  // read with its count, a whole table included.
  const info = await harness.rpc<{ dbPath: string }>('HarnessInfo');
  const schema = await readSchema(info.dbPath);
  const allowlist = [
    { table: 'items', values: { kind: ['assistant_text'] }, reason: 'the spec writes one' },
    { table: 'projects', reason: 'the spec seeds one' },
  ];
  const coverage = await measureCoverage(info.dbPath, schema, allowlist);
  let failure = '';
  try {
    assertCoverage(schema, coverage, allowlist);
  } catch (error) {
    failure = (error as Error).message;
  }
  expect(failure).toContain(`items.kind=assistant_text: the store writes it now; remove it from the entry; it is on (`);
  expect(failure).toMatch(new RegExp(`items\\.kind=assistant_text: [^\\n]*thread title "${title}"`));
  expect(failure).toMatch(new RegExp(`\\n  - projects: the store writes it now; remove the entry; it is on \\([^\\n]*id "${projectId}"`));
});

test('the at-rest check names a thread whose sent message has not opened its turn, and passes once the turn ends', async ({ harness }) => {
  const inherited = storeThreads(harness);
  const title = 'At rest: turn still to open';
  const { cwd, threadId } = await seedThread(harness, title);
  await setScenario(harness, cwd, plainScenario({ name: `at-rest-stalled-${randomBytes(3).toString('hex')}`, provider: 'claude', texts: ['Answered.'] }));
  await harness.rpc('StartSession', threadId);
  await harness.rpc('HarnessStallProviderEvents', threadId);
  let released = false;
  try {
    const ended = awaitTurnCompleted(harness, threadId, TURN_TIMEOUT_MS);
    await harness.rpc('SendMessage', threadId, 'Answer once.', null);
    // The stall itself is one pending event; the turn's frames queue behind it.
    await expect.poll(() => harness.rpc<number>('HarnessPendingProviderEvents', threadId), {
      message: 'the turn\'s frames never reached the stalled worker',
      timeout: TURN_TIMEOUT_MS,
    }).toBeGreaterThan(1);
    await expect(assertAtRest(harness, 'stalled', inherited)).rejects.toThrow(
      new RegExp(`\\n  - "${title}" \\(${threadId}\\): a message is queued or its turn is still to open`));
    await harness.rpc('HarnessReleaseProviderEvents', threadId);
    released = true;
    await ended;
    await assertAtRest(harness, 'handled', inherited);
  } finally {
    if (!released) await harness.rpc('HarnessReleaseProviderEvents', threadId);
  }
});
