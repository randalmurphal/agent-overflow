// The release upgrade check on a data root whose app died with work in
// flight (docs/architecture/release-upgrade-check.md, reading a recovery
// report). A Claude thread is killed with an open turn streaming, a
// background agent running with a tool call of its own in flight, a
// background agent that already finished (its launch keeps running beside
// its completion sibling), a message queued behind the open turn, and a
// terminal thread, which the next boot deletes whole. The root also holds a
// deleted fork source the fork still reads (a holder). The launcher's update
// path then runs on that root, as the check runs it: the snapshot, the trial
// boot, the fingerprint and the census.
//
//   recovery - every change the trial's boot made is one a recovery rule
//              declares, and the report counts it by rule; the census
//              allows exactly the rows and threads those rules removed, and
//              counts threads as the listings do, the holder not at all;
//   altered  - a settled row the boot changed fails the check as the
//              boot's doing, naming the row and its column;
//   deleted  - a settled user row the boot deleted fails it the same way;
//   stamp    - a row revision that moved in a thread the boot wrote nothing
//              in fails it: stamps move only with the writes that cause them;
//   migration - a change the snapshot migrated alone also carries fails it
//              as the migrations' doing, not the boot's.
import { copyFile, mkdir, mkdtemp, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeTurnsScenario,
  emit,
  listItems,
  openTextLines,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  textLines,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import { fingerprint, migrateAlone } from './upgrade-corpus-fingerprint.js';
import { census, compareCensus, recoveryGrowth } from './upgrade-corpus-helpers.js';
import { SHELL_ENV, startTerminalThread } from './terminal-thread-lifetime-flow.js';
import { plainScenario } from './thread-tools-helpers.js';
import { buildSandbox, newUpdateID, runUpdateCommand, updateResult, type Sandbox } from './update-launcher-helpers.js';
import { buildVersion, dbQuery } from './upgrade-corpus-tools.js';

const COMMAND_TIMEOUT_MS = 120_000;
const QUEUED = 'Queued behind the open turn.';

// stageHolder leaves a deleted fork source its fork still reads: a holder,
// a thread row no listing shows, which the census must not count either.
// It runs in its own process, so the crashed one's mock events are its own.
async function stageHolder(rig: Sandbox): Promise<void> {
  const host: HarnessApp = await launchHarness({ dataDir: rig.dataRoot });
  try {
    await host.rpc('HarnessSetScenario', { scenario: plainScenario({ name: 'upgrade-holder', provider: 'claude', texts: ['Answered.'] }) });
    const sourceId = await seedAgentThread(host, 'upgrade-holder', 'Forked, then deleted');
    const answered = host.waitForEvent<{ threadId: string }>('provider:turn_completed', (event) => event.threadId === sourceId);
    await host.rpc('SendMessage', sourceId, 'answer once', null);
    await answered;
    await host.rpc('ForkThread', sourceId, null);
    await host.rpc('DeleteThread', sourceId);
    expect(await dbQuery(rig.dbPath, `SELECT mode FROM threads WHERE id = '${sourceId}'`)).toEqual([{ mode: 'holder' }]);
    const counted = await census(rig.dbPath);
    expect([counted.threads, counted.archived, counted.pinned]).toEqual([
      ((await host.rpc<unknown[] | null>('HarnessListThreadRows')) ?? []).length,
      ((await host.rpc<unknown[] | null>('ListArchivedThreads')) ?? []).length,
      0,
    ]);
  } finally {
    await host.close();
  }
}

// The dead process's root: what it left live, and its settled rows.
async function stageCrashedRoot(rig: Sandbox): Promise<{ threadId: string; quietId: string; terminalId: string }> {
  const host: HarnessApp = await launchHarness({ dataDir: rig.dataRoot, env: SHELL_ENV });
  try {
    const tasks = [
      { task_id: 'task-live', task_type: 'local_agent', description: 'live worker' },
      { task_id: 'task-done', task_type: 'local_agent', description: 'finished worker' },
    ];
    await host.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('upgrade-recovery', [
        [
          emit([
            ...textLines('msg-lead', 'Launching two workers.'),
            toolUseLine('msg-live', 'tu-live', 'Agent', { description: 'live worker', subagent_type: 'worker', prompt: 'Work.' }),
            taskStartedLine('task-live', 'tu-live', 'live worker'),
            asyncAgentAckLine('tu-live', 'task-live', 'live worker'),
            toolUseLine('msg-done', 'tu-done', 'Agent', { description: 'finished worker', subagent_type: 'worker', prompt: 'Finish.' }),
            taskStartedLine('task-done', 'tu-done', 'finished worker'),
            asyncAgentAckLine('tu-done', 'task-done', 'finished worker'),
            backgroundTasksChangedLine(tasks),
            RESULT_LINE,
          ]),
          { waitSignal: { name: 'work' } },
          emit([
            toolUseLine('msg-live-read', 'tu-live-read', 'Read', { file_path: 'README.md' }, 'tu-live'),
            taskNotificationLine('task-done', 'tu-done', 'Finished.'),
          ]),
        ],
        [
          emit(openTextLines('msg-open', 'Thinking about the next step')),
          { waitSignal: { name: 'never' } },
        ],
      ], { holdQueuedInput: true }),
    });
    const threadId = await seedAgentThread(host, 'upgrade-recovery', 'Upgrade recovery');
    const quietId = await seedAgentThread(host, 'upgrade-quiet', 'Settled before the crash');
    const terminal = await startTerminalThread(host, 'Shell of the dead process');
    expect(terminal.mode).toBe('terminal');
    const mockId = await startMock(host, threadId);
    const completed = host.waitForEvent('provider:turn_completed');
    await host.rpc('SendMessage', threadId, 'launch two workers', null);
    await completed;
    await waitForGate(host, 'work');
    await advance(host, mockId, 'work');
    await host.rpc('SendMessage', threadId, 'what next', null);
    await waitForGate(host, 'never');
    await expect.poll(async () => (await listItems(host, threadId))
      .filter((i) => i.completionOf === 'tu-done' || i.id === 'tu-live-read').map((i) => `${i.id}:${i.status}`).sort())
      .toHaveLength(2);
    await host.rpc('RegisterQueueItem', threadId, QUEUED, {});
    await expect.poll(async () => (await dbQuery<{ n: number }>(rig.dbPath, `SELECT
        (SELECT COUNT(*) FROM flush_queue_items) + (SELECT COUNT(*) FROM items WHERE kind = 'user_text'
          AND json_valid(meta) AND json_extract(meta, '$.pendingFlush') = 1) AS n`))[0].n).toBe(1);
    expect(await host.crash()).toBe(true);
    return { threadId, quietId, terminalId: terminal.id };
  } finally {
    await host.close();
  }
}

// copyDatabase copies a closed database's file and WAL into dir.
async function copyDatabase(dbPath: string, dir: string): Promise<string> {
  await mkdir(dir, { recursive: true });
  const base = path.basename(dbPath);
  for (const name of await readdir(path.dirname(dbPath))) {
    if (name === base || name === `${base}-wal`) await copyFile(path.join(path.dirname(dbPath), name), path.join(dir, name));
  }
  return path.join(dir, base);
}

// write runs an edit through the store's own triggers, as a defective
// writer would. Their find-text triggers call the store's SQL functions
// (store/find_fold.go); stand-ins do, since the check does not compare
// the derived column they fill.
function write(dbPath: string, sql: string): void {
  const db = new DatabaseSync(dbPath);
  try {
    db.function('find_loose', (text: unknown) => text as string | null);
    db.function('find_candidate', { varargs: true }, () => null);
    db.exec(sql);
  } finally {
    db.close();
  }
}

test('the release upgrade check tells a dead process\'s recovered rows from altered settled ones', async () => {
  test.setTimeout(5 * 60_000);
  const root = await mkdtemp(path.join(tmpdir(), 'ao-upgrade-recovery-'));
  try {
    const rig = await buildSandbox(root);
    await stageHolder(rig);
    const { threadId, quietId, terminalId } = await stageCrashedRoot(rig);
    const before = await census(rig.dbPath);

    const id = newUpdateID();
    const snapshot = await runUpdateCommand(rig, ['__update-snapshot', '--id', id], COMMAND_TIMEOUT_MS);
    const { schema } = updateResult(snapshot.events) as { schema: number };
    expect(updateResult(snapshot.events), snapshot.stderr).toMatchObject({ outcome: 'ok' });
    const trial = await runUpdateCommand(rig,
      ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1'], COMMAND_TIMEOUT_MS);
    expect(updateResult(trial.events), trial.stderr).toMatchObject({ outcome: 'prepared' });
    const snapshotDB = path.join(rig.dataRoot, 'agent-overflow', 'runtime', 'app-update', 'snapshot', 'agent-overflow.db');
    const migrated = await migrateAlone(snapshotDB, path.join(root, 'migrated-alone'));
    const check = (booted: string, alone = migrated) => fingerprint(booted, snapshotDB, schema, schema, async () => alone);

    // recovery
    const clean = await check(rig.dbPath);
    expect(clean.failures).toEqual([]);
    expect(clean.bootUnexplained).toEqual([]);
    expect(Object.fromEntries(clean.recovery.map((entry) => [`${entry.rule} ${entry.table}`, entry.rows]))).toEqual({
      'terminal-threads thread_search_rows': 1,
      'terminal-threads threads': 1,
      'crashed-turns turns': 1,
      'crashed-turn-rows items': 1,
      'claude-background-launches items': 1,
      'claude-agent-rows items': 1,
      'unconfirmed-queued-messages items': 1,
      'unconfirmed-message-search-rows thread_search_rows': 1,
      'live-agent-cards subagent_aggregates': 1,
      'thread-stamps threads': 1,
    });
    // The census allows what those rules removed: the terminal thread, and
    // the unconfirmed user row of the recovered thread. Without the
    // allowance it would call the recovery a loss.
    expect(clean.recoveryRemoved).toEqual({
      tables: { items: 1, thread_search_rows: 2, threads: 1 }, threadItems: { [threadId]: 1 }, threads: [terminalId],
    });
    const after = await census(rig.dbPath);
    const growth = await recoveryGrowth(snapshotDB, clean.recoveryRemoved);
    expect(growth.exact).toEqual({ threads: -1, archived: 0, pinned: 0 });
    compareCensus(before, after, schema, schema, growth);
    expect(() => compareCensus(before, after, schema, schema)).toThrow(/^threads: /);
    expect(() => compareCensus(before, after, schema, schema, { exact: growth.exact })).toThrow(/^table thread_search_rows shrank/);
    // The trial's session_died sibling refills the thread's count; were the
    // removal its only change, the thread would be one row short.
    const short = { ...after, threadItems: { ...after.threadItems, [threadId]: before.threadItems[threadId] - 1 } };
    compareCensus(before, short, schema, schema, growth);
    expect(() => compareCensus(before, short, schema, schema,
      { exact: growth.exact, removed: { tables: clean.recoveryRemoved.tables, threadItems: {} } })).toThrow(`thread ${threadId} had`);
    expect(() => compareCensus(before, { ...short, threadItems: { ...short.threadItems, [threadId]: before.threadItems[threadId] - 2 } },
      schema, schema, growth)).toThrow(`thread ${threadId} had`);
    // The finished worker's launch keeps running beside its sibling; the boot left it alone.
    const launch = `SELECT status, meta FROM items WHERE thread_id = '${threadId}' AND id = 'tu-done'`;
    expect(await dbQuery(rig.dbPath, launch)).toEqual(await dbQuery(snapshotDB, launch));

    const settled = (await dbQuery<{ id: string; kind: string }>(snapshotDB,
      `SELECT id, kind FROM items WHERE thread_id = '${threadId}' AND turn_index = 0 AND kind IN ('user_text', 'assistant_text')`));
    const settledText = settled.find((row) => row.kind === 'assistant_text')!.id;
    const settledUser = settled.find((row) => row.kind === 'user_text')!.id;

    // altered
    const altered = await copyDatabase(rig.dbPath, path.join(root, 'altered'));
    write(altered, `UPDATE items SET summary = summary || ' (edited)' WHERE thread_id = '${threadId}' AND id = '${settledText}'`);
    const alteredCheck = await check(altered);
    expect(alteredCheck.failures).toHaveLength(1);
    expect(alteredCheck.failures[0]).toContain("the candidate's boot");
    expect(alteredCheck.bootUnexplained).toEqual([
      { table: 'items', key: { thread_id: threadId, id: settledText }, removed: false, columns: ['summary', 'rev'] },
    ]);

    // deleted
    const deleted = await copyDatabase(rig.dbPath, path.join(root, 'deleted'));
    write(deleted, `DELETE FROM items WHERE thread_id = '${threadId}' AND id = '${settledUser}'`);
    const deletedCheck = await check(deleted);
    expect(deletedCheck.failures).toHaveLength(1);
    expect(deletedCheck.failures[0]).toContain("the candidate's boot");
    expect(deletedCheck.bootUnexplained).toEqual([
      { table: 'items', key: { thread_id: threadId, id: settledUser }, removed: true, columns: [] },
    ]);

    // stamp: a revision moves only with a write in its thread.
    const stamped = await copyDatabase(rig.dbPath, path.join(root, 'stamped'));
    write(stamped, `UPDATE items SET rev = rev + 1 WHERE thread_id = '${quietId}' AND kind = 'assistant_text'`);
    const stampedCheck = await check(stamped);
    expect(stampedCheck.failures).toHaveLength(1);
    expect(stampedCheck.bootUnexplained).toEqual([
      { table: 'items', key: { thread_id: quietId, id: expect.any(String) }, removed: false, columns: ['rev'] },
    ]);

    // migration
    const migratedAltered = await copyDatabase(migrated, path.join(root, 'migrated-altered'));
    const edit = `UPDATE items SET summary = summary || ' (migrated)' WHERE thread_id = '${threadId}' AND id = '${settledText}'`;
    write(migratedAltered, edit);
    const both = await copyDatabase(rig.dbPath, path.join(root, 'both-altered'));
    write(both, edit);
    const migrationCheck = await check(both, migratedAltered);
    // The edit stamps the thread too, so both tables name the migrations.
    expect(migrationCheck.failures).toHaveLength(2);
    for (const failure of migrationCheck.failures) {
      expect(failure).toMatch(/^the store open \(no migration was pending\) left 1 pre-migration row\(s\) missing or changed in (items|threads) /);
    }
    expect(Object.entries(migrationCheck.migrationChangedRows!).filter(([, n]) => n > 0)).toEqual([['items', 1], ['threads', 1]]);
    expect(migrationCheck.bootUnexplained).toEqual([]);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
