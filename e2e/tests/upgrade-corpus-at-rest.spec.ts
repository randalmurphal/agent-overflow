// The upgrade corpus top-up's at-rest check (assertAtRest in
// upgrade-corpus-topup.ts), which every top-up step runs when it returns,
// and the coverage gate's naming of the rows a stale allowlist entry is on
// (upgrade-corpus-coverage.ts). A turn held at a gate after its streaming
// row is written, and a sent message whose turn has not opened because
// the backend holds the provider's frames, each fail the check naming
// their thread, and pass it once they end. So do a turn whose end write
// the store refused and a row whose settle it refused, which the backend
// reads at rest: the check's store half names them, through the partial
// indexes of open turns and unsettled rows. An
// entry the store writes after all fails the gate naming the rows that
// carry it, a whole-table entry included.
import { randomBytes } from 'node:crypto';
import * as path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { awaitTurnCompleted, plainScenario, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { assertCoverage, measureCoverage, readSchema } from './upgrade-corpus-coverage.js';
import { OPEN_TURNS_SQL, UNSETTLED_ROWS_SQL, assertAtRest, storeThreads } from './upgrade-corpus-topup.js';

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
  await harness.startSession(threadId);
  const held = harness.waitForEvent<{ mockId: string; cwd: string; report: { kind: string; detail?: string } }>(
    'harness:mock', (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === gate && ev.cwd === cwd, TURN_TIMEOUT_MS);
  await harness.send(threadId, 'Start and stop halfway.');
  const parked = await held;
  await expect.poll(() => streamingRows(harness, threadId), {
    message: 'the held turn never wrote its streaming row',
    timeout: TURN_TIMEOUT_MS,
  }).toBeGreaterThan(0);

  await expect(assertAtRest(harness, 'held', inherited)).rejects.toThrow(
    new RegExp(`top-up step held returned while a turn of its own still runs .*\\n  - "${title}" \\(${threadId}\\): a turn is open`, 's'));

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
  await harness.startSession(threadId);
  await harness.rpc('HarnessStallProviderEvents', threadId);
  let released = false;
  try {
    const ended = awaitTurnCompleted(harness, threadId, TURN_TIMEOUT_MS);
    await harness.send(threadId, 'Answer once.');
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

/**
 * Runs `body` with a trigger on the live store that refuses the writes
 * `when` names on `table`, as a full disk would, and drops it after.
 */
async function refusingWrites(harness: HarnessApp, name: string, table: string, when: string, body: () => Promise<void>): Promise<void> {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'));
  try {
    database.exec('PRAGMA busy_timeout = 10000');
    database.exec(`CREATE TRIGGER "${name}" BEFORE UPDATE ON ${table} WHEN ${when} BEGIN SELECT RAISE(ABORT, 'refused by the spec'); END`);
    try {
      await body();
    } finally {
      database.exec(`DROP TRIGGER IF EXISTS "${name}"`);
    }
  } finally {
    database.close();
  }
}

/** Writes `sql` on the live store, for a settle the backend's own write was refused. */
function writeStore(harness: HarnessApp, sql: string, ...params: string[]): void {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'));
  try {
    database.exec('PRAGMA busy_timeout = 10000');
    database.prepare(sql).run(...params);
  } finally {
    database.close();
  }
}

test('the at-rest check names a turn whose end the store refused, which the backend reads at rest', async ({ harness }) => {
  const inherited = storeThreads(harness);
  const title = 'At rest: turn end refused';
  const { cwd, threadId } = await seedThread(harness, title);
  await setScenario(harness, cwd, plainScenario({ name: `at-rest-refused-${randomBytes(3).toString('hex')}`, provider: 'claude', texts: ['Answered.'] }));
  await harness.startSession(threadId);
  await refusingWrites(harness, `refuse_turn_end_${threadId}`, 'turns',
    `NEW.thread_id = '${threadId}' AND OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL`, async () => {
      const ended = awaitTurnCompleted(harness, threadId, TURN_TIMEOUT_MS);
      await harness.send(threadId, 'Answer once.');
      await ended;
    });
  expect(await harness.rpc('HarnessThreadsWorking', [threadId]), 'the backend no longer reads the turn as live').toEqual([]);
  await expect(assertAtRest(harness, 'refused', inherited)).rejects.toThrow(
    new RegExp(`\\n  - "${title}" \\(${threadId}\\): turn \\d+ is open in the store`));
  // The spec settles the row the backend's write could not.
  writeStore(harness, `UPDATE turns SET completed_at = started_at WHERE thread_id = ? AND completed_at IS NULL`, threadId);
  await assertAtRest(harness, 'settled', inherited);
});

test('the at-rest check names a streaming row whose settle the store refused', async ({ harness }) => {
  const inherited = storeThreads(harness);
  const title = 'At rest: row settle refused';
  const { cwd, threadId } = await seedThread(harness, title);
  await setScenario(harness, cwd, plainScenario({ name: `at-rest-row-${randomBytes(3).toString('hex')}`, provider: 'claude', texts: ['Answered.'] }));
  await harness.startSession(threadId);
  await refusingWrites(harness, `refuse_row_settle_${threadId}`, 'items',
    `NEW.thread_id = '${threadId}' AND OLD.status = 'streaming' AND NEW.status <> 'streaming'`, async () => {
      const ended = awaitTurnCompleted(harness, threadId, TURN_TIMEOUT_MS);
      await harness.send(threadId, 'Answer once.');
      await ended;
    });
  expect(await harness.rpc('HarnessThreadsWorking', [threadId]), 'the backend no longer reads the turn as live').toEqual([]);
  await expect(assertAtRest(harness, 'refused', inherited)).rejects.toThrow(
    new RegExp(`\\n  - "${title}" \\(${threadId}\\): assistant_text row \\S+ is streaming in the store`));
  // The item triggers call functions only the backend registers, so the
  // spec cannot settle the row itself; the next reset deletes the thread.
});

test("the at-rest check's store reads go through the partial indexes of open turns and unsettled rows", async ({ harness }) => {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    const plan = (sql: string) => (database.prepare(`EXPLAIN QUERY PLAN ${sql}`).all('["a","b"]') as Array<{ detail: string }>)
      .map((row) => row.detail).join('\n');
    expect(plan(OPEN_TURNS_SQL)).toMatch(/SEARCH turns USING (COVERING )?INDEX idx_turns_inflight \(thread_id=\?\)/);
    expect(plan(UNSETTLED_ROWS_SQL)).toMatch(/SEARCH items USING INDEX idx_items_unsettled \(thread_id=\?\)/);
  } finally {
    database.close();
  }
});
