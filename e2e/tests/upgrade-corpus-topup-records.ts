// Top-up steps (upgrade-corpus-topup.ts) for the records a turn's rows do
// not carry: review drafts, agent thread-tool requests and the open pull
// request a worktree thread tracks. Each writes every side, scope, state and
// optional column the schema gives these tables, so a migration that
// rebuilds one of them meets rows with each column set.
import { DatabaseSync } from 'node:sqlite';
import path from 'node:path';
import { expect } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { publishBranch, type ForgeRepo } from './forge-helpers.js';
import { trackedRow } from './merge-cleanup-helpers.js';
import { FOOTER_TOKEN_PATTERN, THREAD_TOOLS_SERVER, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

const TURN_TIMEOUT_MS = 120_000;

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
 * two sends one target turn answers, and a reminder that fires. A third
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
    ],
  }));
  const send = (message: string, wait: boolean) => ({
    call: {
      tool: 'thread_send',
      args: { thread_id: target, message, ...(wait ? { wait_seconds: 120 } : { notify: true }) },
      timeoutMs: 180_000,
    },
  });
  await setScenario(harness, cwd, threadToolsScenario({
    name: `topup-caller-${nonce}`,
    provider: 'codex',
    // The caller's wakes are written as the app-server echoes them, which
    // confirms each and names its row (wake_item_id).
    echoUserMessage: true,
    turns: [
      { steps: [send('Where does the top-up stand?', true)], text: 'The other thread replied.' },
      { steps: [send('Anything else?', true)], text: 'The other thread finished.' },
      { steps: [send('First of two.', false), send('Second of two.', false)], text: 'Asked twice.' },
      { text: 'Both answers arrived.' },
      { steps: [{ call: { tool: 'thread_remind', args: { after_seconds: 1, note: `the top-up v${version} reminder` } } }], text: 'Reminder set.' },
      { text: 'Reminded.' },
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
