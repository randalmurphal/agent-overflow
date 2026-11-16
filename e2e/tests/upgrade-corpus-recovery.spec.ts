// The release upgrade check on a data root whose app died with work in
// flight (docs/architecture/release-upgrade-check.md, reading a recovery
// report). A Claude thread is killed with an open turn streaming, a
// background agent running with a tool call of its own in flight, a
// background agent that already finished (its launch keeps running beside
// its completion sibling), and a message queued behind the open turn. The
// dead process also leaves, as store fixtures written after the crash, one
// live row for every other recovery rule (stageFixtures), and threads and
// rows the first ordinary boot after the trial sweeps: a terminal thread, an
// unfinished fork, an unpromoted side chat, a delete in progress whose
// thread a holder's only fork is, and a Pull requests list of a repository
// no project names. The launcher's update path then runs on that root, as
// the check runs it: the snapshot, the trial boot, the fingerprint and the
// census; then a candidate boot sweeps.
//
//   recovery - every change the trial's boot made is one a recovery rule
//              declares, the report counts it by rule, and a change outside
//              each rule fails as the boot's;
//   altered  - a settled row the boot changed fails the check as the
//              boot's doing, naming the row and its column;
//   deleted  - a settled user row the boot deleted fails it the same way;
//   stamp    - a row revision that moved in a thread the boot wrote nothing
//              in fails it: stamps move only with the writes that cause them;
//   migration - a change the snapshot migrated alone also carries fails it
//              as the migrations', and an exception narrows only that side;
//   sweeps   - the boots after the trial remove exactly what the sweep rules
//              select, the census allows exactly that, the reads leave the
//              swept threads out, and a loss outside the rules fails as the
//              boots'.
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
import type { SweepContext } from './upgrade-corpus-config.js';
import { fingerprint, migrateAlone, storeAt, type Fingerprint } from './upgrade-corpus-fingerprint.js';
import {
  census,
  compareCensus,
  expectedCatalog,
  readCatalog,
  sweepBaseline,
  sweepGrowth,
  type Census,
} from './upgrade-corpus-helpers.js';
import { SHELL_ENV, startTerminalThread } from './terminal-thread-lifetime-flow.js';
import { plainScenario } from './thread-tools-helpers.js';
import { buildSandbox, newUpdateID, runUpdateCommand, updateResult, type Sandbox } from './update-launcher-helpers.js';
import { buildVersion, dbQuery } from './upgrade-corpus-tools.js';

const COMMAND_TIMEOUT_MS = 120_000;
const QUEUED = 'Queued behind the open turn.';
const QUEUED_WAKE = 'An answer arrived.';
const TYPED = 'Typed in the composer.';
const REPLACED = 'The text a resend replaced.';
/** The repeated text: queued, and already in the composer. */
const REPEAT = 'Please continue';
/** The padded thread's two queued messages and draft, as the rows hold them, and the draft the restore writes. */
const SPACED = { first: '\n\tqueued text\u00A0\n', second: ' second\t', draft: 'old draft\n' };
const SPACED_DRAFT = 'queued text\n\nsecond\n\nold draft';
/** An SQL string literal. */
const sqlText = (text: string) => `'${text.replaceAll("'", "''")}'`;
/** The second member of the joined unconfirmed message, whose queue copy outlived the write. */
const MEMBER = 'A second message joined to the first.';
/** usermessage.JoinSeparator. */
const JOIN = '\n\n---\n\n';
/** The send of a wake whose message the provider confirmed before the crash. */
const DELIVERED_SEND = 'thread-wake:req-delivered';

interface Staged {
  threadId: string;
  quietId: string;
  codexId: string;
  draftId: string;
  terminalId: string;
  scratchId: string;
  preparingId: string;
  /** A queued message whose text the composer already holds. */
  repeatId: string;
  /** Queued messages and a draft padded with newlines, tabs and a non-breaking space. */
  spacedId: string;
  holderId: string;
  forkId: string;
}

// stageHolder leaves a deleted fork source its fork still reads: a holder,
// a thread row no listing shows, which the census must not count either.
// It runs in its own process, so the crashed one's mock events are its own.
async function stageHolder(rig: Sandbox): Promise<{ holderId: string; forkId: string }> {
  const host: HarnessApp = await launchHarness({ dataDir: rig.dataRoot });
  try {
    await host.rpc('HarnessSetScenario', { scenario: plainScenario({ name: 'upgrade-holder', provider: 'claude', texts: ['Answered.'] }) });
    const holderId = await seedAgentThread(host, 'upgrade-holder', 'Forked, then deleted');
    const answered = host.waitForEvent<{ threadId: string }>('provider:turn_completed', (event) => event.threadId === holderId);
    await host.rpc('SendMessage', holderId, 'answer once', null);
    await answered;
    const fork = await host.rpc<{ id: string }>('ForkThread', holderId, null);
    await host.rpc('DeleteThread', holderId);
    expect(await dbQuery(rig.dbPath, `SELECT mode FROM threads WHERE id = '${holderId}'`)).toEqual([{ mode: 'holder' }]);
    const counted = await census(rig.dbPath);
    expect([counted.threads, counted.archived, counted.pinned]).toEqual([
      ((await host.rpc<unknown[] | null>('HarnessListThreadRows')) ?? []).length,
      ((await host.rpc<unknown[] | null>('ListArchivedThreads')) ?? []).length,
      0,
    ]);
    return { holderId, forkId: fork.id };
  } finally {
    await host.close();
  }
}

// The dead process's root: what it left live, and its settled rows.
async function stageCrashedRoot(rig: Sandbox): Promise<Omit<Staged, 'holderId' | 'forkId'>> {
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
    const codexId = await seedAgentThread(host, 'upgrade-codex', 'Codex worker', 'codex');
    const draftId = await seedAgentThread(host, 'upgrade-draft', 'Resent while running');
    const scratchId = await seedAgentThread(host, 'upgrade-scratch', 'Side chat');
    const preparingId = await seedAgentThread(host, 'upgrade-preparing', 'Fork in preparation');
    const repeatId = await seedAgentThread(host, 'upgrade-repeat', 'Queued what the composer held');
    const spacedId = await seedAgentThread(host, 'upgrade-spaced', 'Queued with padding');
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
    return { threadId, quietId, codexId, draftId, terminalId: terminal.id, scratchId, preparingId, repeatId, spacedId };
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

/** Two 32-byte key thumbprints (base64url, unpadded). */
const MEMBER_KEY = 'A'.repeat(43);
const INTRODUCED_KEY = 'Q'.repeat(43);

// stageFixtures writes, into the dead process's closed store, one live row
// for each recovery rule the crashed turn does not reach, a settled twin
// where a rule may change any column (so a change outside it has a row to
// land on), and the rows the first ordinary boot after the trial sweeps.
// Each is what that writer's own code would have left had the process died
// mid-operation.
async function stageFixtures(rig: Sandbox, s: Staged): Promise<void> {
  const [{ projectId }] = await dbQuery<{ projectId: string }>(rig.dbPath, `SELECT project_id AS projectId FROM threads WHERE id = '${s.codexId}'`);
  const [local] = await dbQuery<{ id: string; user: string; device: string; key: string }>(rig.dbPath,
    `SELECT id, user_id AS user, device_id AS device, signing_key_id AS key FROM sessions WHERE binding_class = 'loopback-only' LIMIT 1`);
  const [{ queuedRow }] = await dbQuery<{ queuedRow: string }>(rig.dbPath, `SELECT id AS queuedRow FROM items
    WHERE thread_id = '${s.threadId}' AND kind = 'user_text' AND json_extract(meta, '$.pendingFlush') = 1`);
  const now = Date.now();
  const later = now + 30 * 86_400_000;
  const session = (id: string, expires: number) => `INSERT INTO sessions (id, user_id, device_id, binding_class, scopes, signing_key_id, created_at, expires_at)
    VALUES ('${id}', '${local.user}', '${local.device}', 'device-bound', '[]', '${local.key}', ${now}, ${expires});`;
  const link = (id: string, fields: string, values: string) => `INSERT INTO pairing_links
    (id, user_id, token_hash, scopes, binding_class, device_class, created_at, expires_at${fields})
    VALUES ('${id}', '${local.user}', randomblob(32), '[]', 'device-bound', 'desktop', ${now}, ${later}${values});`;
  const request = (token: string, caller: string, state: string, extra = '', extraValues = '') => `INSERT INTO thread_requests
    (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at${extra})
    VALUES ('${token}', '${caller}', 'send', '${s.quietId}', '${state}', ${now}, ${now}${extraValues});`;
  write(rig.dbPath, `
    -- codex-background-runtime: a Codex background command still running.
    INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, is_background, tool_name, meta, created_at, updated_at)
      SELECT 'codex-bg', thread_id, turn_index, item_index + 50, 'tool_call', 'assistant', 'running', 'background shell', 1, 'shell', '{}',
             created_at, updated_at FROM items WHERE thread_id = '${s.codexId}' AND kind = 'assistant_text';
    -- worktree-setups: a worktree setup the process was running.
    UPDATE threads SET worktree_setup_state = 'running' WHERE id = '${s.codexId}';
    -- unsettled-review-claims and retired-message-claims: review comments a
    -- message claimed, one waiting on its row and one the unconfirmed
    -- queued message carries.
    INSERT INTO diff_review_comments (id, thread_id, scope, source_key, file_path, status, side, body, sent_at, created_at, updated_at,
        claim_thread_id, claim_item_id, claim_settles)
      VALUES ('drc-unsettled', '${s.codexId}', 'turn', 'k', 'a.go', 'sent', 'file', 'Check this.', ${now}, ${now}, ${now},
        '${s.codexId}', 'u-pending', 'row'),
             ('drc-retired', '${s.threadId}', 'turn', 'k', 'b.go', 'sent', 'file', 'And this.', ${now}, ${now}, ${now},
        '${s.threadId}', '${queuedRow}', '');
    -- open-local-requests, open-receipts and queued-wakes-to-draft: an
    -- agent request still running, a receipt of one, a wake still queued
    -- and the request that rode with it, and settled twins.
    ${request('req-open', s.codexId, 'running')}
    ${request('req-settled', s.codexId, 'finished', ', answer_kind, settled_at', `, 'reply', ${now}`)}
    ${request('req-wake', s.codexId, 'replied', ', answer, answer_kind, settled_at, delivered_at, delivered_how', `, x'01', 'reply', ${now}, ${now}, 'queued'`)}
    ${request('req-rider', s.codexId, 'replied', ', answer, answer_kind, settled_at, delivered_at, delivered_how, delivered_with',
      `, x'01', 'reply', ${now}, ${now}, 'queued', 'req-wake'`)}
    INSERT INTO thread_request_receipts (token, owner_device_id, kind, target_thread_id, state, created_at, updated_at)
      VALUES ('rcpt-open', '${local.device}', 'send', '${s.codexId}', 'running', ${now}, ${now}),
             ('rcpt-settled', '${local.device}', 'send', '${s.codexId}', 'finished', ${now}, ${now});
    INSERT INTO flush_queue_items (id, thread_id, send_id, message, payload, enqueued_at)
      VALUES ('fq-wake', '${s.codexId}', 'thread-wake:req-wake', '${QUEUED_WAKE}', CAST('{"attachmentIds":["att-queued"]}' AS BLOB), ${now});
    -- queued-message-drafts and replacement-drafts: the drafts a restore
    -- writes into, with typed text and an attachment, a recovery a resend
    -- left, and a settled draft.
    INSERT INTO thread_drafts (thread_id, content, attachments, updated_at, has_content)
      VALUES ('${s.codexId}', '${TYPED}', '["att-typed"]', ${now}, 1), ('${s.draftId}', '${TYPED}', '[]', ${now}, 1),
             ('${s.quietId}', 'kept', '[]', ${now}, 1);
    -- The restore's composition: a queued message the draft already holds
    -- word for word, which the draft must then hold twice; and two queued
    -- messages and a draft padded with whitespace, which the restore trims.
    INSERT INTO flush_queue_items (id, thread_id, send_id, message, enqueued_at)
      VALUES ('fq-repeat', '${s.repeatId}', 'send-repeat', ${sqlText(REPEAT)}, ${now}),
             ('fq-spaced-1', '${s.spacedId}', 'send-spaced-1', ${sqlText(SPACED.first)}, ${now}),
             ('fq-spaced-2', '${s.spacedId}', 'send-spaced-2', ${sqlText(SPACED.second)}, ${now + 1});
    INSERT INTO thread_drafts (thread_id, content, attachments, updated_at, has_content)
      VALUES ('${s.repeatId}', ${sqlText(REPEAT)}, '[]', ${now}, 1), ('${s.spacedId}', ${sqlText(SPACED.draft)}, '[]', ${now}, 1);
    INSERT INTO thread_draft_recoveries (thread_id, send_id, content, attachments)
      VALUES ('${s.draftId}', 'send-lost', '${REPLACED}', '[]');
    -- background-terminal-stashes: a background task's end that outran its row.
    INSERT INTO pending_background_task_terminals (thread_id, task_id, tool_use_id, status, source, created_at)
      VALUES ('${s.threadId}', 'task-stash', 'tu-stash', 'completed', 'notification', ${now});
    -- running-remote-jobs.
    INSERT INTO remote_jobs (id, owner_id, fingerprint, source_thread_id, project_id, workspace, state, started_at)
      VALUES ('job-running', 'owner', 'fp', '${s.codexId}', '${projectId}', '/tmp', 'running', ${now});
    -- workflow-runs, -phases, -units: a run mid-phase, and a finished twin.
    INSERT INTO work_items (id, project_id, goal, workflow_id, workflow_scope, snapshot, state, source, created_at, started_at)
      VALUES ('wi-running', '${projectId}', 'Build it', 'build', 'project',
              '{"workflow":{"id":"build","name":"Build","phases":[{"id":"build","driver":"agent","shape":"fan-out"}]}}', 'running', 'manual', ${now}, ${now}),
             ('wi-done', '${projectId}', 'Built it', 'build', 'project', '', 'done', 'manual', ${now}, ${now});
    INSERT INTO work_item_phases (item_id, phase_id, attempt, status, started_at) VALUES ('wi-running', 'build', 1, 'running', ${now});
    INSERT INTO work_item_phases (item_id, phase_id, attempt, status, started_at, ended_at) VALUES ('wi-done', 'build', 1, 'completed', ${now}, ${now});
    INSERT INTO work_item_units (item_id, phase_id, attempt, unit_id, unit_index, kind, status, started_at)
      VALUES ('wi-running', 'build', 1, 'u1', 0, 'unit', 'running', ${now}), ('wi-done', 'build', 1, 'u1', 0, 'unit', 'done', ${now});
    -- usage-attention-claims: a usage-limit notice queued for a thread.
    INSERT INTO workflow_provider_usage_scopes (id, provider, credential_generation, first_seen_at, last_seen_at)
      VALUES (1, 'claude', 1, ${now}, ${now});
    INSERT INTO workflow_provider_usage_attention (scope_id, thread_id, generation, queued_generation, queued_token, source_item_id, updated_at)
      VALUES (1, '${s.codexId}', 1, 1, 'tok-usage', 'src-usage', ${now});
    -- payloads-of-removed-items, chunks-of-removed-payloads: the queued
    -- message's text held in a payload.
    INSERT INTO payloads (thread_id, id, kind, data, created_at) VALUES ('${s.threadId}', 'pl-queued', 'text', CAST('${QUEUED}' AS BLOB), ${now});
    INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at)
      VALUES ('${s.threadId}', 'pl-queued', 0, 0, CAST('${QUEUED}' AS BLOB), ${now});
    UPDATE items SET payload_id = 'pl-queued' WHERE thread_id = '${s.threadId}' AND id = '${queuedRow}';
    -- The unconfirmed message is a joined one (one provider message carrying
    -- two queued ones), and its second member's queue copy outlived the
    -- write: the restore takes the row whole and the copy adds no text.
    UPDATE items SET summary = summary || '${JOIN}' || '${MEMBER}',
        meta = json_set(meta, '$.sendId', 'send-joined-first', '$.joinedSendIds', json_array('send-joined-first', 'send-joined-second'))
      WHERE thread_id = '${s.threadId}' AND id = '${queuedRow}';
    INSERT INTO flush_queue_items (id, thread_id, send_id, message, enqueued_at)
      VALUES ('fq-joined-copy', '${s.threadId}', 'send-joined-second', '${MEMBER}', ${now});
    -- A wake's queue row whose message the provider had confirmed (its row
    -- carries the provider's id): the restore drops it with no text and
    -- leaves the request as delivered.
    UPDATE items SET meta = json_set(meta, '$.sendId', '${DELIVERED_SEND}', '$.provider_item_id', 'msg-delivered')
      WHERE thread_id = '${s.quietId}' AND kind = 'user_text' AND parent_id = '';
    ${request('req-delivered', s.quietId, 'replied', ', answer, answer_kind, settled_at, delivered_at, delivered_how',
      `, x'01', 'reply', ${now}, ${now}, 'queued'`)}
    INSERT INTO flush_queue_items (id, thread_id, send_id, message, enqueued_at)
      VALUES ('fq-delivered', '${s.quietId}', '${DELIVERED_SEND}', 'An answer the provider confirmed.', ${now});
    -- A stale pendingFlush mark on a settled user row a later turn follows:
    -- history, which v152 clears and the boot's queue restore leaves alone.
    UPDATE items SET meta = json_set(meta, '$.pendingFlush', 1)
      WHERE thread_id = '${s.threadId}' AND turn_index = 0 AND kind = 'user_text' AND parent_id = '';
    -- local-session-scopes: the local session at scopes an older build granted.
    UPDATE sessions SET scopes = '[]' WHERE id = '${local.id}';
    -- expired-sessions, -refresh-secrets, -pairing-links, and live twins.
    ${session('sess-expired', 1000)}
    ${session('sess-live', later)}
    INSERT INTO refresh_secrets (id, session_id, secret_hash, created_at, expires_at)
      VALUES ('rs-expired', 'sess-live', randomblob(32), 1000, 1000), ('rs-live', 'sess-live', randomblob(32), ${now}, ${later});
    INSERT INTO pairing_links (id, user_id, token_hash, scopes, binding_class, device_class, created_at, expires_at)
      VALUES ('pl-expired', '${local.user}', randomblob(32), '[]', 'device-bound', 'phone', 1000, 1000);
    -- own-admission-members and own-admission-revocations: two approved
    -- own-device admissions the process did not finish, one it can finish
    -- (an existing member at an older generation) and one it must refuse.
    ${session('sess-member', later)}
    ${session('sess-refused', later)}
    INSERT INTO own_devices (key_thumbprint, member, generation, removed)
      VALUES ('${MEMBER_KEY}', '{"keyThumbprint":"${MEMBER_KEY}","name":"Laptop","routes":[],"generation":1,"removed":false}', 1, 0);
    ${link('pl-member', ', device_id, key_thumbprint, session_id, confirmed_at, purpose, member_generation',
      `, '${local.device}', '${MEMBER_KEY}', 'sess-member', ${now}, 'own-device', 2`)}
    ${link('pl-refused', ', device_id, key_thumbprint, session_id, confirmed_at, purpose, member_generation, sponsor_key',
      `, '${local.device}', '${INTRODUCED_KEY}', 'sess-refused', ${now}, 'own-introduction', 1, '${MEMBER_KEY}'`)}
    -- The sweeps: an unpromoted side chat, an unfinished fork, a delete in
    -- progress on the holder's only fork, and a Pull requests list of a
    -- repository no project names.
    UPDATE threads SET mode = 'scratch' WHERE id = '${s.scratchId}';
    INSERT INTO scratch_threads (thread_id, source_thread_id, return_mode, created_at) VALUES ('${s.scratchId}', '${s.quietId}', 'chat', ${now});
    UPDATE threads SET fork_preparing = 1 WHERE id = '${s.preparingId}';
    UPDATE threads SET deleting = 1 WHERE id = '${s.forkId}';
    INSERT INTO pr_list_projects (repo_key, project_json, read_at) VALUES ('github:octo/gone',
      CAST('{"projectId":"${projectId}","name":"gone","repo":{"Forge":"github","Host":"github.com","Namespace":"octo","Repo":"gone"},"rows":[]}' AS BLOB), ${now});
  `);
}

/** The recovery report as `rule table` to rows. */
const byRule = (fp: Fingerprint) => Object.fromEntries(fp.recovery.map((entry) => [`${entry.rule} ${entry.table}`, entry.rows]));

test('the release upgrade check tells a dead process\'s recovered rows from altered settled ones', async () => {
  test.setTimeout(6 * 60_000);
  const root = await mkdtemp(path.join(tmpdir(), 'ao-upgrade-recovery-'));
  try {
    const rig = await buildSandbox(root);
    const { holderId, forkId } = await stageHolder(rig);
    const staged: Staged = { ...await stageCrashedRoot(rig), holderId, forkId };
    const { threadId, quietId, codexId } = staged;
    await stageFixtures(rig, staged);
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
    // The restore's draft: the queued message ahead of what the composer held.
    expect(await dbQuery(rig.dbPath, `SELECT content, attachments FROM thread_drafts WHERE thread_id = '${codexId}'`))
      .toEqual([{ content: `${QUEUED_WAKE}\n\n${TYPED}`, attachments: '["att-queued","att-typed"]' }]);
    // The joined row whole, its member's queue copy adding nothing; the
    // confirmed message's queue row gone with the draft and wake untouched.
    expect(await dbQuery(rig.dbPath, `SELECT content FROM thread_drafts WHERE thread_id = '${threadId}'`))
      .toEqual([{ content: `${QUEUED}${JOIN}${MEMBER}` }]);
    expect(await dbQuery(rig.dbPath, `SELECT
        (SELECT COUNT(*) FROM flush_queue_items) AS queued,
        (SELECT content FROM thread_drafts WHERE thread_id = '${quietId}') AS draft,
        (SELECT delivered_how FROM thread_requests WHERE token = 'req-delivered') AS how`))
      .toEqual([{ queued: 0, draft: 'kept', how: 'queued' }]);
    // MergeParts' composition: a repeated text twice, padding trimmed.
    expect(await dbQuery(rig.dbPath, `SELECT thread_id AS thread, content FROM thread_drafts
        WHERE thread_id IN ('${staged.repeatId}', '${staged.spacedId}') ORDER BY thread_id = '${staged.spacedId}'`))
      .toEqual([{ thread: staged.repeatId, content: `${REPEAT}\n\n${REPEAT}` }, { thread: staged.spacedId, content: SPACED_DRAFT }]);
    expect(byRule(clean)).toEqual({
      'crashed-turns turns': 1,
      'crashed-turn-rows items': 1,
      'codex-background-runtime items': 1,
      'claude-background-launches items': 1,
      'claude-agent-rows items': 1,
      'unconfirmed-queued-messages items': 1,
      'unconfirmed-message-search-rows thread_search_rows': 1,
      'payloads-of-removed-items payloads': 1,
      'chunks-of-removed-payloads payload_chunks': 1,
      'live-agent-cards subagent_aggregates': 1,
      // The Codex thread's stamps move with its worktree setup's row, which that rule counts.
      'thread-stamps threads': 1,
      'worktree-setups threads': 1,
      'queued-messages flush_queue_items': 6,
      'queued-message-drafts thread_drafts': 3,
      'queued-wakes-to-draft thread_requests': 2,
      'retired-message-claims diff_review_comments': 1,
      'unsettled-review-claims diff_review_comments': 1,
      'replacement-draft-recoveries thread_draft_recoveries': 1,
      'replacement-drafts thread_drafts': 1,
      'open-receipts thread_request_receipts': 1,
      'open-local-requests thread_requests': 1,
      'background-terminal-stashes pending_background_task_terminals': 1,
      'running-remote-jobs remote_jobs': 1,
      'workflow-runs work_items': 1,
      'workflow-run-phases work_item_phases': 1,
      'workflow-run-units work_item_units': 1,
      'usage-attention-claims workflow_provider_usage_attention': 1,
      'local-session-scopes sessions': 1,
      'own-admission-members own_devices': 1,
      'own-admission-revocations sessions': 1,
      'expired-sessions sessions': 1,
      'expired-refresh-secrets refresh_secrets': 1,
      'expired-pairing-links pairing_links': 1,
    });
    // The finished worker's launch keeps running beside its sibling; the boot left it alone.
    const launch = `SELECT status, meta FROM items WHERE thread_id = '${threadId}' AND id = 'tu-done'`;
    expect(await dbQuery(rig.dbPath, launch)).toEqual(await dbQuery(snapshotDB, launch));

    // Every rule fails a change outside it: a column it may not change, a
    // row it does not select, or a row it removes kept with a change.
    const outside: Array<{ rule: string; table: string; sql: string }> = [
      { rule: 'crashed-turns', table: 'turns', sql: `UPDATE turns SET error_message = 'edited' WHERE thread_id = '${threadId}' AND turn_index = 1` },
      { rule: 'codex-background-runtime', table: 'items', sql: `UPDATE items SET tool_name = 'other' WHERE thread_id = '${codexId}' AND id = 'codex-bg'` },
      { rule: 'worktree-setups', table: 'threads', sql: `UPDATE threads SET worktree_setup_state = '' WHERE id = '${codexId}'` },
      { rule: 'unsettled-review-claims', table: 'diff_review_comments', sql: `UPDATE diff_review_comments SET body = 'edited' WHERE id = 'drc-unsettled'` },
      { rule: 'retired-message-claims', table: 'diff_review_comments', sql: `UPDATE diff_review_comments SET body = 'edited' WHERE id = 'drc-retired'` },
      { rule: 'open-local-requests', table: 'thread_requests', sql: `UPDATE thread_requests SET issue = 'edited' WHERE token = 'req-settled'` },
      { rule: 'open-receipts', table: 'thread_request_receipts', sql: `UPDATE thread_request_receipts SET source_thread_title = 'edited' WHERE token = 'rcpt-settled'` },
      { rule: 'queued-wakes-to-draft', table: 'thread_requests', sql: `UPDATE thread_requests SET issue = 'edited' WHERE token = 'req-rider'` },
      { rule: 'queued-messages', table: 'flush_queue_items', sql: `INSERT INTO flush_queue_items SELECT id, thread_id, send_id, 'edited', payload, enqueued_at FROM snap.flush_queue_items` },
      // A restore's destination draft must hold the old draft and every
      // restored message, text and attachments.
      { rule: 'queued-messages: draft missing', table: 'flush_queue_items', sql: `DELETE FROM thread_drafts WHERE thread_id = '${codexId}'` },
      { rule: 'queued-message-drafts: draft holds only the queued message', table: 'thread_drafts',
        sql: `UPDATE thread_drafts SET content = '${QUEUED_WAKE}', attachments = '["att-queued"]' WHERE thread_id = '${codexId}'` },
      { rule: 'queued-messages: draft holds only the old draft', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = '${TYPED}', attachments = '["att-typed"]' WHERE thread_id = '${codexId}'` },
      { rule: 'queued-messages: the queued attachment lost', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET attachments = '["att-typed"]' WHERE thread_id = '${codexId}'` },
      { rule: 'queued-message-drafts: the old attachment lost', table: 'thread_drafts',
        sql: `UPDATE thread_drafts SET attachments = '["att-queued"]' WHERE thread_id = '${codexId}'` },
      { rule: 'queued-message-drafts: attachments corrupted', table: 'thread_drafts',
        sql: `UPDATE thread_drafts SET attachments = 'att-queued,att-typed' WHERE thread_id = '${codexId}'` },
      { rule: 'unconfirmed-queued-messages: draft missing', table: 'items', sql: `DELETE FROM thread_drafts WHERE thread_id = '${threadId}'` },
      { rule: 'queued-messages: a joined copy gone, the draft holding only its own text', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = '${MEMBER}' WHERE thread_id = '${threadId}'` },
      { rule: 'queued-messages: dropped as delivered with no confirmed row and no draft text', table: 'flush_queue_items',
        sql: `UPDATE items SET meta = json_remove(meta, '$.provider_item_id') WHERE thread_id = '${quietId}' AND json_extract(meta, '$.sendId') = '${DELIVERED_SEND}'` },
      { rule: 'queued-wakes-to-draft: a confirmed wake marked as restored', table: 'thread_requests',
        sql: `UPDATE thread_requests SET delivered_how = 'draft' WHERE token = 'req-delivered'` },
      { rule: 'replacement-drafts: draft holds only the old draft', table: 'thread_draft_recoveries',
        sql: `UPDATE thread_drafts SET content = '${TYPED}' WHERE thread_id = '${staged.draftId}'` },
      { rule: 'queued-messages: a repeated text lost, the draft left as it was', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = ${sqlText(REPEAT)} WHERE thread_id = '${staged.repeatId}'` },
      { rule: 'queued-messages: the restored parts in the wrong order', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = ${sqlText('second\n\nqueued text\n\nold draft')} WHERE thread_id = '${staged.spacedId}'` },
      { rule: 'queued-messages: a part restored twice', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = ${sqlText(`queued text\n\n${SPACED_DRAFT}`)} WHERE thread_id = '${staged.spacedId}'` },
      { rule: 'queued-messages: the padding kept', table: 'flush_queue_items',
        sql: `UPDATE thread_drafts SET content = ${sqlText(`${SPACED.first}\n\n${SPACED.second}\n\n${SPACED.draft}`)} WHERE thread_id = '${staged.spacedId}'` },
      { rule: 'replacement-drafts: draft holds only the replaced text', table: 'thread_drafts',
        sql: `UPDATE thread_drafts SET content = '${REPLACED}' WHERE thread_id = '${staged.draftId}'` },
      { rule: 'retired-message-claims: resolved, not returned', table: 'diff_review_comments',
        sql: `UPDATE diff_review_comments SET status = 'resolved' WHERE id = 'drc-retired'` },
      { rule: 'unsettled-review-claims: resolved, not returned', table: 'diff_review_comments',
        sql: `UPDATE diff_review_comments SET status = 'resolved' WHERE id = 'drc-unsettled'` },
      { rule: 'replacement-draft-recoveries', table: 'thread_draft_recoveries', sql: `INSERT INTO thread_draft_recoveries SELECT thread_id, send_id, 'edited', attachments FROM snap.thread_draft_recoveries` },
      { rule: 'background-terminal-stashes', table: 'pending_background_task_terminals', sql: `INSERT INTO pending_background_task_terminals SELECT thread_id, task_id, tool_use_id, 'failed', exit_code, output_file, end_time, source, created_at FROM snap.pending_background_task_terminals` },
      { rule: 'running-remote-jobs', table: 'remote_jobs', sql: `UPDATE remote_jobs SET output = 'edited' WHERE id = 'job-running'` },
      { rule: 'workflow-runs', table: 'work_items', sql: `UPDATE work_items SET goal = 'edited' WHERE id = 'wi-done'` },
      { rule: 'workflow-run-phases', table: 'work_item_phases', sql: `UPDATE work_item_phases SET park_cause = 'edited' WHERE item_id = 'wi-done'` },
      { rule: 'workflow-run-units', table: 'work_item_units', sql: `UPDATE work_item_units SET feedback = 'edited' WHERE item_id = 'wi-done'` },
      { rule: 'local-session-scopes', table: 'sessions', sql: `UPDATE sessions SET activated_at = 1 WHERE binding_class = 'loopback-only'` },
      { rule: 'usage-attention-claims', table: 'workflow_provider_usage_attention', sql: `UPDATE workflow_provider_usage_attention SET generation = 7` },
      { rule: 'payloads-of-removed-items', table: 'payloads', sql: `INSERT INTO payloads SELECT thread_id, id, 'edited', meta, data, created_at, preview_spans, spans FROM snap.payloads WHERE id = 'pl-queued'` },
      { rule: 'own-admission-members', table: 'own_devices', sql: `UPDATE own_devices SET removed = 1 WHERE key_thumbprint = '${MEMBER_KEY}'` },
      { rule: 'own-admission-revocations', table: 'sessions', sql: `UPDATE sessions SET scopes = '["edited"]' WHERE id = 'sess-refused'` },
      { rule: 'expired-sessions', table: 'sessions', sql: `DELETE FROM sessions WHERE id = 'sess-live'` },
      { rule: 'expired-refresh-secrets', table: 'refresh_secrets', sql: `DELETE FROM refresh_secrets WHERE id = 'rs-live'` },
      { rule: 'expired-pairing-links', table: 'pairing_links', sql: `DELETE FROM pairing_links WHERE id = 'pl-member'` },
    ];
    for (const [i, change] of outside.entries()) {
      const copy = await copyDatabase(rig.dbPath, path.join(root, `outside-${i}`));
      const db = new DatabaseSync(copy);
      try {
        db.function('find_loose', (text: unknown) => text as string | null);
        db.function('find_candidate', { varargs: true }, () => null);
        db.exec(`ATTACH '${snapshotDB}' AS snap`);
        db.exec(change.sql);
      } finally {
        db.close();
      }
      const result = await check(copy);
      expect(result.failures, change.rule).toHaveLength(1);
      expect(result.failures[0], change.rule).toContain("the candidate's boot");
      expect(result.bootUnexplained.map((row) => row.table), change.rule).toContain(change.table);
    }

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
    expect(deletedCheck.bootUnexplained).toContainEqual(
      { table: 'items', key: { thread_id: threadId, id: settledUser }, removed: true, columns: [] });

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
    // In a thread the boot wrote nothing in, so only the migrated copy explains it.
    const edit = `UPDATE items SET summary = summary || ' (migrated)' WHERE thread_id = '${quietId}' AND kind = 'assistant_text'`;
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

    // An exception narrows only the migrations' side: v152's mark removal,
    // which the store's triggers restamp, is reported under its exception,
    // while the same columns changed by the boot alone still fail as the boot's.
    const clearMark = `UPDATE items SET meta = json_remove(meta, '$.pendingFlush') WHERE thread_id = '${threadId}' AND id = '${settledUser}'`;
    const migratedCleared = await copyDatabase(migrated, path.join(root, 'migrated-cleared'));
    write(migratedCleared, clearMark);
    const bootCleared = await copyDatabase(rig.dbPath, path.join(root, 'boot-cleared'));
    write(bootCleared, clearMark);
    const v152 = (booted: string, alone: string) => fingerprint(booted, snapshotDB, schema - 1, schema, async () => alone);
    const excepted = await v152(bootCleared, migratedCleared);
    expect(excepted.failures).toEqual([]);
    expect(excepted.bootUnexplained).toEqual([]);
    expect(excepted.migrationExcepted).toEqual({ items: 1, threads: 1 });
    write(bootCleared, `UPDATE items SET meta = json_set(meta, '$.edited', 1) WHERE thread_id = '${quietId}' AND kind = 'assistant_text'`);
    const bootMeta = await v152(bootCleared, migratedCleared);
    expect(bootMeta.failures).toHaveLength(1);
    expect(bootMeta.failures[0]).toContain("the candidate's boot");
    expect(bootMeta.bootUnexplained).toEqual([
      { table: 'items', key: { thread_id: quietId, id: expect.any(String) }, removed: false, columns: ['meta', 'rev'] },
    ]);

    // The census across the trial allows exactly what the recovery rules removed.
    const postTrial = await census(rig.dbPath);
    compareCensus(before, postTrial, schema, schema, 'migration', { removed: clean.recoveryRemoved });
    expect(() => compareCensus(before, postTrial, schema, schema, 'migration')).toThrow(/across the migrations/);
    const short: Census = { ...postTrial, threadItems: { ...postTrial.threadItems, [threadId]: before.threadItems[threadId] - 1 } };
    compareCensus(before, short, schema, schema, 'migration', { removed: clean.recoveryRemoved });
    expect(() => compareCensus(before, { ...short, threadItems: { ...short.threadItems, [threadId]: before.threadItems[threadId] - 2 } },
      schema, schema, 'migration', { removed: clean.recoveryRemoved })).toThrow(`thread ${threadId} had`);

    // sweeps
    const swept = await sweepBaseline(rig.dbPath);
    expect(Object.fromEntries(Object.entries(swept.threads).map(([thread, entry]) => [thread, entry.rule]))).toEqual({
      [staged.terminalId]: 'terminal-threads',
      [staged.preparingId]: 'interrupted-forks',
      [staged.scratchId]: 'scratch-threads',
      [forkId]: 'pending-thread-deletes',
      [holderId]: 'released-holders',
    });
    // A read that lands before a sweep finishes leaves out the threads it is
    // removing, whichever side of the sweep the read falls on.
    const unswept = { rpc: async (method: string) => method === 'HarnessListThreadRows' ? [{ id: staged.scratchId }, { id: quietId }] : null };
    expect(await readCatalog(unswept as unknown as HarnessApp, swept)).toMatchObject({ threads: 1 });
    const candidate = await launchHarness({ dataDir: rig.dataRoot, env: SHELL_ENV });
    let live: SweepContext;
    try {
      live = { forgeProjects: (await candidate.rpc<SweepContext['forgeProjects'] | null>('ListForgeProjects')) ?? [] };
      expect(await readCatalog(candidate, swept)).toEqual(expectedCatalog(postTrial, swept));
      // The catalog reads above settle the first reads, which the delete
      // walk and the Pull requests prefetch wait for.
      await candidate.rpc('ListThreads');
      await expect.poll(async () => (await dbQuery<{ n: number }>(rig.dbPath, `SELECT
          (SELECT COUNT(*) FROM threads WHERE id IN (${Object.keys(swept.threads).map((t) => `'${t}'`).join(', ')}))
          + (SELECT COUNT(*) FROM pr_list_projects) AS n`))[0].n, { timeout: 60_000 }).toBe(0);
    } finally {
      expect(await candidate.stop()).toBe(true);
    }
    const after = await census(rig.dbPath);
    const sweeps = await sweepGrowth(swept, rig.dbPath, live);
    expect(Object.fromEntries(sweeps.report.map((entry) => [entry.rule, [entry.threads, entry.gone]]))).toEqual({
      'terminal-threads': [1, 1],
      'interrupted-forks': [1, 1],
      'scratch-threads': [1, 1],
      'pending-thread-deletes': [1, 1],
      'released-holders': [1, 1],
      'unprojected-pr-lists': [0, 0],
    });
    expect(sweeps.report.find((entry) => entry.rule === 'unprojected-pr-lists')!.rows).toBe(1);
    compareCensus(postTrial, after, schema, schema, 'boot', sweeps.growth);
    expect(() => compareCensus(postTrial, after, schema, schema, 'boot')).toThrow(/across the boots after the trial/);
    // A row its rule keeps: the Pull requests list of a repository a project still names.
    const named = await sweepGrowth(swept, rig.dbPath, {
      forgeProjects: [{ projectId: 'p', repo: { Forge: 'github', Host: 'github.com', Namespace: 'octo', Repo: 'gone' } }],
    });
    expect(() => compareCensus(postTrial, after, schema, schema, 'boot', named.growth))
      .toThrow(/^table pr_list_projects shrank from 1 to 0 rows across the boots after the trial; a boot writer/);
    // A thread no rule selects that loses a row.
    const lost: Census = { ...after, threadItems: { ...after.threadItems, [quietId]: postTrial.threadItems[quietId] - 1 } };
    expect(() => compareCensus(postTrial, lost, schema, schema, 'boot', sweeps.growth)).toThrow(`thread ${quietId} had`);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

// A root from a release older than a column a recovery rule names: v148,
// before thread_requests.delivered_with (v149), with an open local request
// the dead process left running. The trial migrates it and its boot settles
// the request; the fingerprint must classify that settlement over the store
// the migrations left, not fail on the snapshot's older schema.
const BEFORE_WAKE_CARRIER = 148;

test('the release upgrade check classifies a boot recovery on a snapshot older than the rules', async () => {
  test.setTimeout(3 * 60_000);
  const root = await mkdtemp(path.join(tmpdir(), 'ao-upgrade-recovery-old-'));
  try {
    const rig = await buildSandbox(root);
    await storeAt(rig.dbPath, BEFORE_WAKE_CARRIER);
    const workspace = path.join(rig.dataRoot, 'workspaces', 'old');
    await mkdir(workspace, { recursive: true });
    write(rig.dbPath, `
      INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p-old', '${workspace}', 'Old', 1, 1);
      INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
        VALUES ('t-old', 'p-old', 'Caller', 'claude', '${workspace}', 'test-model', 1, 1, 0, 'chat');
      INSERT INTO thread_requests (token, caller_thread_id, kind, state, created_at, updated_at)
        VALUES ('req-old', 't-old', 'send', 'running', 1, 1);
    `);
    const id = newUpdateID();
    const snapshot = await runUpdateCommand(rig, ['__update-snapshot', '--id', id], COMMAND_TIMEOUT_MS);
    expect(updateResult(snapshot.events), snapshot.stderr).toMatchObject({ outcome: 'ok', schema: BEFORE_WAKE_CARRIER });
    const trial = await runUpdateCommand(rig,
      ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1'], COMMAND_TIMEOUT_MS);
    expect(updateResult(trial.events), trial.stderr).toMatchObject({ outcome: 'prepared' });
    const snapshotDB = path.join(rig.dataRoot, 'agent-overflow', 'runtime', 'app-update', 'snapshot', 'agent-overflow.db');
    const [{ schema }] = await dbQuery<{ schema: number }>(rig.dbPath, 'SELECT MAX(version) AS schema FROM migration_versions');
    const result = await fingerprint(rig.dbPath, snapshotDB, BEFORE_WAKE_CARRIER, schema,
      () => migrateAlone(snapshotDB, path.join(root, 'migrated-alone')));
    expect(result.failures).toEqual([]);
    expect(Object.fromEntries(result.recovery.map((entry) => [entry.rule, entry.rows]))).toMatchObject({ 'open-local-requests': 1 });
    expect(await dbQuery(rig.dbPath, `SELECT state FROM thread_requests WHERE token = 'req-old'`)).toEqual([{ state: 'interrupted' }]);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
