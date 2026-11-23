// Top-up steps (upgrade-corpus-topup.ts) for the records a turn's rows do
// not carry: review drafts, agent thread-tool requests, the receipts of a
// paired computer's spawns and the open pull request a worktree thread
// tracks. Each writes every side, scope, state and
// optional column the schema gives these tables, so a migration that
// rebuilds one of them meets rows with each column set.
import { DatabaseSync } from 'node:sqlite';
import path from 'node:path';
import { expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { publishBranch, type ForgeRepo } from './forge-helpers.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { trackedRow } from './merge-cleanup-helpers.js';
import { FOOTER_TOKEN_PATTERN, THREAD_TOOLS_SERVER, awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

const TURN_TIMEOUT_MS = 120_000;

/** The mock gates writePeerSpawns holds two workers' turns at. */
export interface PeerSpawnHolds {
  /** The Codex worker's spawn turn, whose run stays open. */
  spawn: string;
  /** The nudged worker's nudge turn, whose run stays open. */
  nudge: string;
}

/**
 * Review drafts on `threadId`, a thread whose session takes more turns: a
 * range on each half and on a context line, a file comment and a held reply,
 * one in each scope; one sent in a message that claims it, one marked sent
 * and then resolved; and drafts a PR owns with no thread, one of them sent.
 */
export async function writeReviewDrafts(harness: HarnessApp, threadId: string, version: string, nonce: string): Promise<void> {
  const source = `topup-${nonce}`;
  const body = (what: string) => `Top-up v${version}: ${what}`;
  const create = (input: Record<string, unknown>) =>
    harness.rpc<{ id: string }>('CreateDiffReviewComment', threadId, { sourceKey: source, selectedText: '', ...input });

  await create({
    scope: 'turn', filePath: 'src/app.txt', side: 'new', startNewLine: 1, newLine: 3,
    commitSha: 'a1b2c3d', selectedText: 'one\ntwo\nthree', body: body('a range on the new side'),
  });
  const claimed = await create({ scope: 'session', filePath: 'src/app.txt', side: 'old', startOldLine: 2, oldLine: 4, body: body('a range on the old side') });
  const answered = harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === threadId, TURN_TIMEOUT_MS);
  await harness.rpc('SendDiffReviewComments', threadId, 'session', source, [claimed.id], {});
  await answered;
  const resolved = await create({
    scope: 'workspace', filePath: 'src/app.txt', side: 'context', startOldLine: 4, startNewLine: 5, oldLine: 5, newLine: 6,
    body: body('a range of context lines'),
  });
  await harness.rpc('MarkDiffReviewCommentsSent', threadId, 'workspace', source, [resolved.id], `topup-turn-${nonce}`);
  await harness.rpc('DeleteDiffReviewComment', threadId, resolved.id);
  await create({ scope: 'branch', filePath: 'README.md', side: 'file', body: body('a file comment') });
  // The Edits view shows an edit's new side only.
  await create({ scope: 'edits', filePath: 'src/app.txt', side: 'new', newLine: 2, body: body("a comment on an agent's edit") });
  await create({ scope: 'pr', filePath: 'README.md', side: 'reply', replyThreadId: `PRRT_topup_${nonce}`, replyRootId: 4242, body: body('a held reply') });

  // The Pull requests page's drafts on a PR it opened with no thread.
  const pr = { Forge: 'github', Host: 'github.com', Namespace: 'ao-corpus', Repo: `topup-v${version}`, Number: 7 };
  const createForPR = (input: Record<string, unknown>) =>
    harness.rpc<{ id: string }>('CreatePRDiffReviewComment', pr, { scope: 'pr', sourceKey: source, selectedText: '', ...input });
  await createForPR({ filePath: 'README.md', side: 'new', newLine: 1, body: body("a PR's own draft") });
  const posted = await createForPR({ filePath: '', side: 'reply', replyThreadId: `PRRT_topup_pr_${nonce}`, replyRootId: 99, body: body("a PR's reply, posted") });
  await harness.rpc('MarkPRDiffReviewCommentsSent', pr, 'pr', source, [posted.id]);
}

/**
 * Agent requests between two threads of a project of its own: a send its
 * target answers with thread_reply, a send it answers by ending its turn,
 * two sends one target turn answers, a reminder that fires, and a send
 * whose turn the caller's thread_cancel by thread id interrupts. A third
 * thread there works in a worktree whose branch has an open pull request on
 * the forge, which the app tracks. Returns the project's threads.
 */
export async function writeThreadRequests(harness: HarnessApp, version: string, nonce: string): Promise<string[]> {
  const branch = `topup/pr-${nonce}`;
  const name = `topup-requests-v${version}-${nonce}`;
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name,
      repo: { commits: [{ message: 'init', files: { 'README.md': `# Top-up requests v${version}\n` } }], branches: [branch] },
      threads: [
        { title: `Top-up v${version}: asks another thread`, provider: 'codex' },
        { title: `Top-up v${version}: answers a thread`, provider: 'claude' },
        { title: `Top-up v${version}: on an open pull request`, provider: 'claude' },
      ],
    }],
  });
  const [{ projectId, path: cwd, threadIds }] = seeded.projects;
  const [caller, target, onPR] = threadIds;

  // Scenario rules are per provider and workspace, so the Codex caller and
  // the Claude target in one project each run their own script. The target
  // holds what arrives mid-turn, so its third turn can take two sends at once.
  const targetBusy = `topup-target-busy-${nonce}`;
  const targetHeld = `topup-target-held-${nonce}`;
  await setScenario(harness, cwd, threadToolsScenario({
    name: `topup-target-${nonce}`,
    provider: 'claude',
    holdQueuedInput: true,
    turns: [
      {
        steps: [
          { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
          { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: `The top-up v${version} reply.` } } },
        ],
        text: 'Replied to the caller.',
      },
      { text: 'Ended the turn without a reply.' },
      { steps: [{ gate: targetBusy }, { drainQueued: true }], text: 'Answered both at once.' },
      // The caller's thread_cancel interrupts this turn, so its text is
      // never reached.
      { steps: [{ gate: targetHeld }], text: 'Finished the long job.' },
    ],
  }));
  const send = (message: string, wait: boolean) => ({
    call: {
      tool: 'thread_send',
      args: { thread_id: target, message, ...(wait ? { wait_seconds: 120 } : { notify: true }) },
      timeoutMs: 180_000,
    },
  });
  // The caller echoes each message as a real app-server does, so its wakes
  // are confirmed and name the row that carried them (wake_item_id).
  await setScenario(harness, cwd, threadToolsScenario({
    name: `topup-caller-${nonce}`,
    provider: 'codex',
    echoUserMessage: true,
    turns: [
      { steps: [send('Where does the top-up stand?', true)], text: 'The other thread replied.' },
      { steps: [send('Anything else?', true)], text: 'The other thread finished.' },
      { steps: [send('First of two.', false), send('Second of two.', false)], text: 'Asked twice.' },
      { text: 'Both answers arrived.' },
      { steps: [{ call: { tool: 'thread_remind', args: { after_seconds: 1, note: `the top-up v${version} reminder` } } }], text: 'Reminder set.' },
      { text: 'Reminded.' },
      {
        steps: [{ call: { tool: 'thread_send', args: { thread_id: target, message: 'Start the long job.', wait_seconds: 0 } } }],
        text: 'Started it.',
      },
      { steps: [{ call: { tool: 'thread_cancel', args: { thread_id: target } } }], text: 'Stopped it.' },
    ],
  }));

  const turnEnded = (threadId: string) =>
    harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === threadId, TURN_TIMEOUT_MS);
  const turn = async (message: string, tools: string[]) => {
    const ended = turnEnded(caller);
    await harness.rpc('SendMessage', caller, message, null);
    for (const tool of tools) {
      const answer = await harness.awaitMcpResult({ server: THREAD_TOOLS_SERVER, tool, cwd, timeoutMs: TURN_TIMEOUT_MS });
      if (answer.report.isError) throw new Error(`top-up ${tool}: refused: ${answer.report.result ?? ''}`);
    }
    await ended;
  };
  await harness.rpc('StartSession', caller);
  await turn('Ask the other thread.', ['thread_send']);
  await turn('Ask it once more.', ['thread_send']);

  // Two sends one target turn answers settle together, and their caller is
  // told once: one wake carries both, and the request that rode along names
  // the other's message.
  await harness.rpc('SendMessage', target, 'Hold on to what arrives next.', null);
  const parked = await harness.waitForEvent<{ mockId: string; cwd: string; report: { kind: string; detail?: string } }>(
    'harness:mock', (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === targetBusy && ev.cwd === cwd, TURN_TIMEOUT_MS);
  await turn('Ask it twice without waiting.', ['thread_send', 'thread_send']);
  await expect.poll(async () => {
    const live = await harness.rpc<{ flushedItems?: unknown[] }>('GetThreadLiveState', target);
    return live.flushedItems?.length ?? 0;
  }, { message: 'top-up: the two sends never reached the busy target', timeout: TURN_TIMEOUT_MS }).toBe(2);
  const woken = turnEnded(caller);
  await harness.rpc('HarnessMockCommand', parked.mockId, { type: 'advance', name: targetBusy });
  await woken;
  const riders = callerRequests(harness, caller, "delivered_with <> ''");
  if (riders !== 1) throw new Error(`top-up: one wake should have carried both answers; ${riders} request(s) name another's message`);

  await turn('Remind yourself.', ['thread_remind']);
  // The reminder's wake is a turn of its own.
  await turnEnded(caller);

  // An interruption with a cause and its argument: the caller's
  // thread_cancel by thread id stops the turn its send started, which
  // settles the request and its receipt `interrupted` by that thread.
  const held = harness.waitForEvent<{ cwd: string; report: { kind: string; detail?: string } }>(
    'harness:mock', (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === targetHeld && ev.cwd === cwd, TURN_TIMEOUT_MS);
  await turn('Give the other thread the long job.', ['thread_send']);
  await held;
  await turn('Stop it.', ['thread_cancel']);
  const stoppedBy = `"Top-up v${version}: asks another thread" (${caller})`;
  for (const [table, side] of [['thread_requests', 'caller_thread_id'], ['thread_request_receipts', 'source_thread_id']]) {
    await expect.poll(() => interruptions(harness, table, side, caller), {
      message: `top-up: ${table} never recorded the thread_cancel's interruption`,
      timeout: TURN_TIMEOUT_MS,
    }).toEqual([{ state: 'interrupted', answer_kind: 'error', answer_cause: 'thread_cancel', answer_cause_arg: stoppedBy }]);
  }

  const bound = await harness.rpc<{ worktreePath?: string }>('AttachThreadWorktree', onPR, branch);
  if (!bound.worktreePath) throw new Error(`top-up: AttachThreadWorktree bound no worktree for ${branch}`);
  const repo: ForgeRepo = {
    forge: 'github',
    project: `ao-corpus/${name}`,
    pulls: [{ number: 12, title: `Top-up v${version} pull request`, headRef: branch, baseRef: 'main' }],
  };
  await publishBranch(harness, bound.worktreePath, repo, branch);
  // Git status looks the branch's pull request up, and the app records it.
  await expect.poll(async () => {
    await harness.rpc('GetGitStatus', { projectId, workspacePath: bound.worktreePath });
    return trackedRow(harness, onPR)?.number ?? 0;
  }, { message: 'top-up: the worktree thread never tracked its open pull request', timeout: TURN_TIMEOUT_MS }).toBe(12);
  return threadIds;
}

/**
 * Three workers a paired computer spawns into a project of `harness`'s own,
 * whose receipts here carry the run each spawn tracks: two end at rest and
 * record their figures (run_open 0, run_tool_calls and the rest), the other
 * keeps an hourly loop armed, so its run stays open (run_open 1). The
 * spawning computer is a backend of the top-up's own, closed once the
 * receipts are written and its pairing has renewed its session (the spent
 * refresh secret); its pairing stays in the store. The person here
 * then nudges both rested workers, and each nudge's run is kept for the
 * spawn's link (thread_receipt_nudges) until that computer takes it, which
 * it never does once closed: one ends at rest with its figures (run_open
 * 0), the other arms a loop, so it stays open (run_open 1). Returns the
 * project and the workers' threads, each at rest.
 *
 * `hold` names the mock gates two of those turns stop at after the loop
 * each arms, with their runs already open: the Codex worker's spawn turn
 * and the nudged worker's nudge turn. Only a spec that proves the step
 * waits for them passes it (upgrade-corpus-peer-spawns.spec.ts).
 */
export async function writePeerSpawns(harness: HarnessApp, version: string, nonce: string, hold?: PeerSpawnHolds): Promise<{ projectId: string; threadIds: string[] }> {
  const name = `topup-peer-spawns-v${version}-${nonce}`;
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name, repo: { commits: [{ message: 'init', files: { 'README.md': `# Top-up peer spawns v${version}\n` } }] }, threads: [] }],
  });
  const [{ projectId, path: cwd }] = seeded.projects;
  const workers: string[] = [];
  const peer = await launchHarness();
  try {
    const computerId = await pairOwnDevice(peer, harness, `Top-up v${version} computer`);
    const callerSeed = await peer.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `topup-spawner-${nonce}`, repo: {}, threads: [{ title: `Top-up v${version}: spawns on a paired computer`, provider: 'claude' }] }],
    });
    const [{ path: callerCwd, threadIds: [caller] }] = callerSeed.projects;
    // Scenario rules are per provider and workspace, so the Claude workers
    // and the Codex worker in one project each run their own script. A
    // Claude worker's turns count from its session's start: its spawn is
    // the first, and a nudge in the same session the second, which arms a
    // loop; a nudge after a new session start is a first turn again.
    await setScenario(harness, cwd, threadToolsScenario({
      name: `topup-peer-ended-${nonce}`,
      provider: 'claude',
      turns: [
        { text: 'Finished over here.' },
        {
          steps: [
            { call: { tool: 'thread_loop', args: { label: 'peer-notes', note: 'look at the notes', every_seconds: 3600 } } },
            ...(hold ? [{ gate: hold.nudge }] : []),
          ],
          text: 'Watching the notes.',
        },
      ],
      afterTurns: 'repeatLast',
    }));
    await setScenario(harness, cwd, threadToolsScenario({
      name: `topup-peer-open-${nonce}`,
      provider: 'codex',
      turns: [{
        steps: [
          { call: { tool: 'thread_loop', args: { label: 'peer-board', note: 'look at the board', every_seconds: 3600 } } },
          ...(hold ? [{ gate: hold.spawn }] : []),
        ],
        text: 'Watching the board.',
      }],
      afterTurns: 'silent',
    }));
    const spawn = (title: string, provider: string) => ({
      call: {
        tool: 'thread_spawn',
        args: { prompt: `Top-up v${version}: ${title}.`, title: `Top-up v${version}: ${title}`, provider, computer_id: computerId, project_id: projectId, wait_seconds: 0 },
        timeoutMs: TURN_TIMEOUT_MS,
      },
    });
    await setScenario(peer, callerCwd, threadToolsScenario({
      name: `topup-peer-caller-${nonce}`,
      provider: 'claude',
      turns: [
        {
          steps: [spawn('a spawn whose run ends', 'claude'), spawn('a spawn whose run stays open', 'codex'), spawn('a spawn the person nudges', 'claude')],
          text: 'Started all three over there.',
        },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await peer.rpc('StartSession', caller);
    await peer.rpc('SendMessage', caller, 'Start all three on the other computer.', null);
    for (const run of ['ends', 'stays open', 'is nudged']) {
      const answer = await awaitToolAnswer<{ thread_id: string }>(peer, { tool: 'thread_spawn', cwd: callerCwd, timeoutMs: TURN_TIMEOUT_MS });
      if (answer.isError || !answer.value) throw new Error(`top-up: the paired spawn whose run ${run} was refused: ${answer.text}`);
      workers.push(answer.value.thread_id);
    }
    // The open run is recorded at the spawn, so the receipts alone do not
    // say the Codex worker's turn has ended. The harness log keeps a turn
    // that ended before its spawn answered; taking each here also keeps the
    // nudges' wait below from matching a spawn's turn.
    for (const worker of workers) await awaitTurnCompleted(harness, worker, TURN_TIMEOUT_MS);
    await expect.poll(() => receiptRuns(harness, workers), {
      message: "top-up: the paired spawns' receipts never recorded two ended runs and one open run",
      timeout: TURN_TIMEOUT_MS,
    }).toEqual([{ run_open: 0, figures: 1 }, { run_open: 1, figures: 0 }, { run_open: 0, figures: 1 }]);
    // The paired computer's first renewal activates its pairing and spends
    // the refresh secret its redemption was issued. It probes for that on
    // an interval and closing it ends the probe, so the renewal is awaited.
    // A store an earlier top-up wrote keeps that pairing's spent secrets,
    // so only this computer's count.
    await expect.poll(() => renewals(harness, workers), {
      message: 'top-up: the paired computer never renewed the session its pairing opened',
      timeout: TURN_TIMEOUT_MS,
    }).toBeGreaterThan(0);
  } finally {
    await peer.close();
  }
  // The person's messages to the rested owned workers are nudges. The
  // first worker's new session runs its first turn, which ends; the
  // third's second turn arms a loop, which keeps its nudge open.
  const [ended, , nudged] = workers;
  await harness.rpc('StopSession', ended);
  await harness.rpc('StartSession', ended);
  // The open nudge run is recorded when the message is taken, so the
  // receipts alone do not say the second worker's turn has ended.
  const nudgesAnswered = Promise.all([ended, nudged].map((worker) => awaitTurnCompleted(harness, worker, TURN_TIMEOUT_MS)));
  await harness.rpc('SendMessage', ended, 'Check the notes once more.', null);
  await harness.rpc('SendMessage', nudged, 'Keep an eye on the notes.', null);
  await nudgesAnswered;
  await expect.poll(() => receiptNudges(harness, [ended, nudged]), {
    message: "top-up: the paired spawns' links never kept one ended and one open nudge run",
    timeout: TURN_TIMEOUT_MS,
  }).toEqual([{ run_open: 0, figures: 1 }, { run_open: 1, figures: 0 }]);
  return { projectId, threadIds: workers };
}

/** The run each of `workers`' spawn receipts records, in their order: open or not, and whether its figures are written. */
function receiptRuns(harness: HarnessApp, workers: string[]): Array<{ run_open: number | null; figures: number }> {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return workers.map((worker) => {
      const row = database.prepare(`SELECT run_open,
          (run_tool_calls IS NOT NULL AND run_tokens IS NOT NULL AND run_turn_ms IS NOT NULL) AS figures
        FROM thread_request_receipts WHERE target_thread_id = ? AND kind = 'spawn'`).get(worker) as { run_open: number | null; figures: number } | undefined;
      return row ? { run_open: row.run_open, figures: row.figures } : { run_open: null, figures: 0 };
    });
  } finally {
    database.close();
  }
}

/**
 * How many refresh secrets a renewal of the paired computer that spawned
 * `workers` spent here, its device named by their spawn receipts: the spent
 * secret names its successor and the device key that spent it.
 */
function renewals(harness: HarnessApp, workers: string[]): number {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    const row = database.prepare(`SELECT count(*) AS n FROM refresh_secrets r
        JOIN sessions s ON s.id = r.session_id
      WHERE s.device_id IN (SELECT owner_device_id FROM thread_request_receipts
                             WHERE kind = 'spawn' AND target_thread_id IN (SELECT value FROM json_each(?)))
        AND r.consumed_at IS NOT NULL AND r.consumed_by IS NOT NULL AND r.next_secret_hash IS NOT NULL`)
      .get(JSON.stringify(workers)) as { n: number };
    return row.n;
  } finally {
    database.close();
  }
}

/** The latest nudge run kept for each of `workers`' spawn links, in their order: open or not, and whether its end and figures are written. */
function receiptNudges(harness: HarnessApp, workers: string[]): Array<{ run_open: number | null; figures: number }> {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return workers.map((worker) => {
      const row = database.prepare(`SELECT n.run_open,
          (n.ended_at IS NOT NULL AND n.run_tool_calls IS NOT NULL AND n.run_tokens IS NOT NULL AND n.run_turn_ms IS NOT NULL) AS figures
        FROM thread_receipt_nudges n JOIN thread_request_receipts link ON link.token = n.link_token
        WHERE n.target_thread_id = ? AND link.kind = 'spawn' AND link.spawn_link = 1
        ORDER BY n.started_at DESC LIMIT 1`).get(worker) as { run_open: number; figures: number } | undefined;
      return row ? { run_open: row.run_open, figures: row.figures } : { run_open: null, figures: 0 };
    });
  } finally {
    database.close();
  }
}

/** The interrupted rows of `caller`'s requests in `table`, whose `side` column names the caller. */
function interruptions(harness: HarnessApp, table: string, side: string, caller: string): Array<Record<string, unknown>> {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return database.prepare(`SELECT state, answer_kind, answer_cause, answer_cause_arg FROM ${table}
      WHERE ${side} = ? AND state = 'interrupted'`).all(caller).map((row) => ({ ...row }));
  } finally {
    database.close();
  }
}

/** How many of `caller`'s requests match `where`, read from the store. */
function callerRequests(harness: HarnessApp, caller: string, where: string): number {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    const row = database.prepare(`SELECT count(*) AS n FROM thread_requests WHERE caller_thread_id = ? AND ${where}`).get(caller) as { n: number };
    return row.n;
  } finally {
    database.close();
  }
}
