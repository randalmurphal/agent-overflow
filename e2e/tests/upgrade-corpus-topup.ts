// The top-up: the slice of use the candidate writes into the corpus at its
// own schema. The release check runs it after the migration so the corpus
// the next release inherits carries rows every release since the feature
// landed has written; the coverage gate runs it on a fresh store to prove
// it still writes what the schema can hold. The steps are fixed: one thread
// per library scenario in topUpScenarios, driven through the mock provider
// the way a user's turn runs, then the sidebar and workflow operations.
import { execFileSync } from 'node:child_process';
import { writeFile } from 'node:fs/promises';
import { DatabaseSync } from 'node:sqlite';
import { randomBytes, randomUUID } from 'node:crypto';
import * as path from 'node:path';
import { expect } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { topUpScenarios, type TopUpScenario } from './upgrade-corpus-config.js';
import type { CensusGrowth } from './upgrade-corpus-helpers.js';
import { writePeerSpawns, writeReviewDrafts, writeThreadRequests } from './upgrade-corpus-topup-records.js';
import { dbQuery, timed } from './upgrade-corpus-tools.js';
import { compactBoundaryLine } from './agent-visibility-helpers.js';
import { BOARD_ID, JIRA_ACCOUNT, JIRA_SITE, JIRA_TOKEN, SPRINT_ID, seedFakeJira, type TrackerConnection } from './tracker-helpers.js';
import { awaitToolAnswer, awaitTurnCompleted, plainScenario, threadToolsScenario } from './thread-tools-helpers.js';
import {
  doneResult,
  humanGateWorkflow,
  questionResult,
  seedWorkflowProject,
  setClaudeScenario,
  singlePhaseWorkflow,
  startWorkflow,
} from './workflows-helpers.js';

const TURN_TIMEOUT_MS = 120_000;
const WORKFLOW_TIMEOUT_MS = 120_000;
const MODEL = { claude: 'claude-opus-4-7', codex: 'gpt-5.5' } as const;
/** The catalog model that declares the ultra effort (internal/provider/models.go). */
const ULTRA_CODEX_MODEL = 'gpt-5.6-sol';
/** A 1x1 PNG: the attachment the draft carries. */
const PNG_BYTES = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==',
  'base64',
);

export interface TopUp {
  projectId: string;
  /** What the top-up adds to the entities a census counts, for the check that follows it. */
  growth: CensusGrowth;
  /** Every thread the top-up created, scenario threads, the fork and the side chat included. */
  threadIds: string[];
  /** Scenario name to the thread its turn ran in. */
  scenarioThreads: Record<string, string>;
  /** The work item the workflow run finished. */
  workItemId: string;
  /** Milliseconds per step, for the report. */
  ms: Record<string, number>;
}

interface LibraryEntry {
  name: string;
  provider: 'claude' | 'codex';
}

interface MockEvent {
  mockId: string;
  report: { kind: string; detail?: string };
}

interface ApprovalEvent {
  action: string;
  request?: { requestId: string; threadId: string };
}

/**
 * topUp writes the fixed slice into `harness`'s store. `version` names the
 * candidate, so the project the slice lands in says which release wrote it.
 */
export async function topUp(harness: HarnessApp, version: string): Promise<TopUp> {
  const ms: Record<string, number> = {};
  // Every step returns with the turns it started at rest (assertAtRest).
  // A thread the store held before the top-up is not one of its own: in
  // the release check a loop an earlier top-up armed can wake it.
  const inherited = storeThreads(harness);
  // `atRest` is what the checks took together.
  ms.atRest = 0;
  const step = async <T>(name: string, run: () => Promise<T>): Promise<T> => {
    const done = await timed(run);
    ms[name] = done.ms;
    ms.atRest += (await timed(() => assertAtRest(harness, name, inherited))).ms;
    return done.value;
  };
  const library = await harness.rpc<{ library: LibraryEntry[] }>('HarnessListScenarios');
  const providers = new Map(library.library.map((entry) => [entry.name, entry.provider]));

  // One project per candidate version, reused when a run at the same
  // version already made it (a re-run, or a pull request before the
  // version bump).
  const project = await step('project', () => ensureTopUpProject(harness, version));
  const unverified = await step('unverifiedProject', () => ensureUnverifiedProject(harness, version));
  const { projectId } = project;
  const nonce = randomBytes(3).toString('hex');

  const scenarioThreads: Record<string, string> = {};
  const threadIds: string[] = [];
  for (const scenario of topUpScenarios) {
    const provider = providers.get(scenario.name);
    if (!provider) throw new Error(`topUpScenarios names ${scenario.name}, which the library does not have`);
    const key = scenario.approve === 'deny' ? `${scenario.name}:deny` : scenario.name;
    const thread = await step(`turn:${key}`, () => runScenarioTurn(harness, projectId, scenario, provider, version));
    scenarioThreads[key] = thread;
    threadIds.push(thread);
  }

  await step('sidebar', async () => {
    for (const forked of ['tool-call', 'file-edit-diff']) {
      const fork = await harness.rpc<{ id: string }>('ForkThread', scenarioThreads[forked], null);
      threadIds.push(fork.id);
    }
    // A side chat lives until it is promoted or the next boot sweeps it;
    // promoted, it stays a thread of the corpus.
    const sideChat = await harness.rpc<{ id: string }>('ForkSideChat', scenarioThreads['streaming-text']);
    await harness.rpc('PromoteScratchThread', sideChat.id);
    threadIds.push(sideChat.id);

    // A draft with an image, a file and a terminal capture attached.
    const drafted = scenarioThreads['thinking-then-text'];
    const image = await uploadAttachment(harness, drafted, 'topup.png', 'image/png', PNG_BYTES);
    const file = await uploadAttachment(harness, drafted, 'topup.txt', 'text/plain', Buffer.from(`Top-up v${version}\n`));
    await harness.rpc('SaveDraft', drafted, `Top-up draft v${version} [Image #1] [File #2]`, [image, file],
      [{ id: 'capture', label: 'shell', preview: 'captured', content: 'captured output', createdAt: 1 }], null);
    // The thumbnail is generated and cached on the row at first request.
    await harness.rpc('GetAttachmentThumbnail', drafted, image);

    // A second Codex turn, then the first message reverted and resent edited.
    const reverted = scenarioThreads['codex-revert-paginated'];
    const second = harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === reverted, TURN_TIMEOUT_MS);
    await harness.send(reverted, 'Second top-up question.');
    await second;
    const rows = await harness.items<{ id: string; kind: string; summary: string }>(reverted, false);
    const target = rows.find((row) => row.kind === 'user_text' && row.summary === 'Second top-up question.');
    if (!target) throw new Error('codex-revert-paginated: the second user message is not in the timeline');
    const resent = harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === reverted, TURN_TIMEOUT_MS);
    await harness.rpc('RevertConversationAndResendMessage', reverted, target.id, { content: 'Second top-up question, rephrased.' });
    await resent;

    // A composer shell command: its output is stored as a file attachment
    // that records the command, and sent as one more message of the thread.
    const shellSent = harness.waitForEvent<{ threadId?: string; phase?: string; state?: string; error?: string }>(
      'shell:command', (ev) => ev.threadId === reverted && ev.phase === 'finished', TURN_TIMEOUT_MS);
    const shellAnswered = harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === reverted, TURN_TIMEOUT_MS);
    await harness.rpc('RunThreadShellCommand', reverted, { runId: randomUUID(), command: `echo "top-up v${version}"` });
    const shellResult = await shellSent;
    if (shellResult.state !== 'sent') throw new Error(`codex-revert-paginated: the shell command ended ${shellResult.state}: ${shellResult.error ?? ''}`);
    await shellAnswered;

    await harness.rpc('PinThread', scenarioThreads['tool-call']);
    await harness.rpc('PinThread', scenarioThreads['file-edit']);
    await harness.rpc('SetThreadPinGroup', scenarioThreads['file-edit'], 1);
    const group = await harness.rpc<{ id: string }>('CreateThreadGroup', projectId, `Top-up v${version} ${nonce}`);
    await harness.rpc('SetThreadGroup', [scenarioThreads['codex-basic'], scenarioThreads['codex-approval']], group.id);
    await harness.rpc('ArchiveThread', scenarioThreads['session-death']);
    await harness.rpc('RenameThread', scenarioThreads['usage-limit-claude'], `Top-up v${version}: renamed after its usage limit`);
    await harness.rpc('SetChatBarFavorite', { kind: 'model', provider: 'claude', value: MODEL.claude, label: 'Opus' }, true);
    // Two assistants made by New Assistant, one serving every project and
    // one moved to a new thread by New Thread, Same Assistant, which leaves
    // a past thread. Each opening and handoff message takes a turn. A new
    // assistant takes the provider of the thread created last, so a Claude
    // thread goes first.
    const remembered = await harness.rpc<{ id: string }>('CreateThread', {
      projectId,
      title: `Top-up v${version}: the profile New Assistant takes`,
      provider: 'claude',
      model: MODEL.claude,
      mode: 'chat',
    });
    threadIds.push(remembered.id);
    await harness.rpc('HarnessSetScenario', {
      scenario: threadToolsScenario({ name: 'topup-assistants', provider: 'claude', turns: [{ text: 'Noted.' }], afterTurns: 'repeatLast' }),
    });
    const assistant = async (name: string, allProjects = false): Promise<string> => {
      const created = await harness.rpc<{ id: string }>('CreateAssistant', { name, projectIds: allProjects ? [] : [projectId], allProjects });
      await awaitTurnCompleted(harness, created.id, TURN_TIMEOUT_MS);
      threadIds.push(created.id);
      return created.id;
    };
    await assistant(`Top-up v${version} ${nonce}: every project`, true);
    const moved = await assistant(`Top-up v${version} ${nonce}: moved on`);
    // Unsent text with an attachment, which the handoff moves to the new
    // thread and records on the past thread's row.
    const note = await uploadAttachment(harness, moved, 'handoff.txt', 'text/plain', Buffer.from(`Handoff v${version}\n`));
    await harness.rpc('SaveDraft', moved, `Top-up handoff draft v${version} [File #1]`, [note], [], null);
    const successor = await harness.rpc<{ id: string }>('StartNextAssistantThread', moved);
    await awaitTurnCompleted(harness, successor.id, TURN_TIMEOUT_MS);
    threadIds.push(successor.id);
    // An assistant that spawned a helper of its own work and set itself a
    // loop and a watch through the thread tools, an hour out, and stopped a
    // second loop. The helper runs on Codex in the home, from a script of
    // its own. The hourly watch is run once now and keeps its stderr, and
    // a second hourly one, run once in a missing directory, keeps its
    // failure notice; a
    // short watch fires once, and the turn its wake starts cancels it
    // before a second fire is due; a long-running watch is started and
    // stopped. Nothing is left running for a later boot of the corpus to
    // restart.
    const agent = { id: await assistant(`Top-up v${version} ${nonce}: loop and watch`) };
    const record = await harness.rpc<{ home: string }>('GetStandingAgent', agent.id);
    const home = record.home;
    // The scenario below applies to the session it starts.
    await harness.stopSession(agent.id);
    await harness.rpc('HarnessSetScenario', {
      cwd: home,
      scenario: plainScenario({ name: 'topup-helper', provider: 'codex', texts: ['Collected the notes.', 'Added your note.'] }),
    });
    await harness.rpc('HarnessSetScenario', {
      scenario: threadToolsScenario({
        name: 'topup-wakes',
        provider: 'claude',
        turns: [{
          steps: [
            { call: { tool: 'thread_spawn', args: { prompt: 'Collect the inbox notes.', title: `Top-up v${version}: helper`, provider: 'codex', instructions: 'Keep each note to one line.', wait_seconds: 0 } } },
            { call: { tool: 'thread_loop', args: { label: 'board', note: 'look at the board', every_seconds: 3600 } } },
            // A run prints nothing on stdout, so it wakes nobody, and the
            // line on stderr is kept on the row (last_stderr).
            { call: { tool: 'thread_watch', args: { label: 'inbox', command: 'echo "inbox: nothing new" >&2', cwd: home, every_seconds: 3600, timeout_seconds: 30 } } },
            // Its directory does not exist, so its run cannot start and the
            // watch raises its failure notice (failure_noticed_at).
            { call: { tool: 'thread_watch', args: { label: 'mailbox', command: 'true', cwd: path.join(home, 'mailbox'), every_seconds: 3600 } } },
            { call: { tool: 'thread_loop', args: { label: 'retired', note: 'an old check', every_seconds: 3600 } } },
            { call: { tool: 'thread_loop', args: { label: 'retired', stop: true } } },
            {
              call: {
                tool: 'thread_watch',
                args: {
                  label: 'once',
                  command: 'echo "once: checked the inbox" >&2; [ -e once.done ] || { : > once.done; echo found; }',
                  cwd: home,
                  every_seconds: 10,
                  timeout_seconds: 30,
                },
              },
            },
            { call: { tool: 'thread_watch', args: { label: 'tail', command: 'sleep 600', cwd: home, long_running: true } } },
            { call: { tool: 'thread_watch', args: { label: 'tail', stop: true } } },
          ],
          text: 'Scheduled.',
        }, {
          // The turn compacts first, so the corpus holds a compaction row.
          // It then flags the thread for the user (attention_kind and the
          // rest).
          steps: [
            { emitLines: [compactBoundaryLine('topup-compaction-${TURN}')] },
            { call: { tool: 'thread_cancel', args: { label: 'once' } } },
            { call: { tool: 'thread_attention', args: { kind: 'review', reason: 'The inbox has a note for you' } } },
          ],
          text: 'Stopped the one-off watch.',
        }],
        afterTurns: 'silent',
      }),
    });
    await harness.startSession(agent.id);
    const scheduled = awaitTurnCompleted(harness, agent.id, TURN_TIMEOUT_MS);
    await harness.send(agent.id, 'Look at the board and the inbox every hour.');
    const spawned = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', timeoutMs: TURN_TIMEOUT_MS });
    if (spawned.isError || !spawned.value) throw new Error(`top-up thread_spawn of a helper was refused: ${spawned.text}`);
    await awaitTurnCompleted(harness, spawned.value.thread_id, TURN_TIMEOUT_MS);
    threadIds.push(spawned.value.thread_id);
    for (const label of ['inbox', 'mailbox', 'once', 'tail', 'tail stopped']) {
      const answer = await awaitToolAnswer(harness, { tool: 'thread_watch', timeoutMs: TURN_TIMEOUT_MS });
      if (answer.isError) throw new Error(`top-up thread_watch ${label} was refused: ${answer.text}`);
    }
    await scheduled;
    // The short watch's one fire is waited for by its effect: the turn its
    // wake starts cancels the watch.
    const cancelled = await awaitToolAnswer(harness, { tool: 'thread_cancel', timeoutMs: TURN_TIMEOUT_MS });
    if (cancelled.isError) throw new Error(`top-up thread_cancel was refused: ${cancelled.text}`);
    const flagged = await awaitToolAnswer(harness, { tool: 'thread_attention', timeoutMs: TURN_TIMEOUT_MS });
    if (flagged.isError) throw new Error(`top-up thread_attention was refused: ${flagged.text}`);
    await awaitTurnCompleted(harness, agent.id, TURN_TIMEOUT_MS);
    // The person's message to the helper, which has no sidebar row, is a
    // nudge: its work is a run of the assistant's (thread_requests.nudge).
    const nudged = awaitTurnCompleted(harness, spawned.value.thread_id, TURN_TIMEOUT_MS);
    await harness.send(spawned.value.thread_id, 'Add one more note.');
    await nudged;
    // A cancel keeps nothing the row carried, so the hourly watch, run once
    // now, is the row that keeps a stderr tail. It stays armed an hour out.
    // The mailbox watch, run once now, cannot start, so it keeps its
    // failure notice; it too stays armed an hour out.
    type WakeRow = { id: string; label: string; state: string; lastRunAt?: number; lastOutcome?: string };
    const watch = async (label: string) =>
      (await harness.rpc<{ rows: WakeRow[] }>('ListThreadWakes', agent.id)).rows.find((row) => row.label === label);
    for (const label of ['inbox', 'mailbox']) {
      const armed = await watch(label);
      if (!armed) throw new Error(`top-up watch ${label} is not listed`);
      await harness.rpc('WakeThreadWakeNow', agent.id, 'watch', armed.id);
      await expect
        .poll(async () => {
          const row = await watch(label);
          return row?.state === 'armed' && (row.lastRunAt ?? 0) > 0;
        }, { timeout: TURN_TIMEOUT_MS })
        .toBe(true);
    }
    const failed = await watch('mailbox');
    if (!failed?.lastOutcome?.startsWith('error: ')) throw new Error(`top-up watch mailbox ran without failing: ${failed?.lastOutcome}`);

    // Threads at every chat-bar setting the schema enumerates, drafts only.
    // Claude fast mode is offered only on a model the CLI reports fast, so
    // the fast variant waits for the startup probe's report.
    await expect.poll(async () => {
      const catalog = await harness.rpc<{ provenance: string }>('GetModelsForProvider', 'claude');
      return catalog.provenance;
    }, { timeout: TURN_TIMEOUT_MS }).toBe('probed');
    for (const variant of THREAD_VARIANTS) {
      const thread = await harness.rpc<{ id: string }>('CreateThread', {
        projectId,
        title: `Top-up v${version}: ${variant.title}`,
        provider: variant.provider ?? 'claude',
        model: variant.model ?? MODEL[variant.provider ?? 'claude'],
        ...variant.options,
        ...(variant.options.worktreeBranch ? { worktreeBranch: `${variant.options.worktreeBranch as string}-${nonce}` } : {}),
      });
      threadIds.push(thread.id);
    }
  });

  await step('reviews', () => writeReviewDrafts(harness, scenarioThreads['codex-revert-paginated'], version, nonce));

  const workItemId = await step('workflow', async () => {
    const done = [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }];
    await setClaudeScenario(harness, 'topup-workflow', done);
    const item = await startWorkflow(harness, projectId, 'topup-flow', `Top-up run v${version}`);
    await awaitWorkflowState(harness, 'workflow run', item.id, 'done');

    // A gated run approved by a human, a run that asks a question, and a
    // run cancelled while stepping.
    const gated = await startWorkflow(harness, projectId, 'topup-gate', `Top-up gated run v${version}`);
    await awaitWorkflowState(harness, 'gated run', gated.id, 'needs-human', 'gate');
    await harness.rpc('WorkflowResolveGate', gated.id, 'approve', 'approved by the top-up');
    await awaitWorkflowState(harness, 'gated run', gated.id, 'done');

    await setClaudeScenario(harness, 'topup-question', [
      { steps: [{ emit: { lines: [questionResult('Which branch should the top-up use?')] } }] },
      ...done,
    ]);
    const asked = await startWorkflow(harness, projectId, 'topup-flow', `Top-up questioned run v${version}`);
    await awaitWorkflowState(harness, 'questioned run', asked.id, 'needs-human', 'question');
    await harness.rpc('WorkflowAnswerQuestion', asked.id, 'main');
    await awaitWorkflowState(harness, 'questioned run', asked.id, 'done');

    const stepped = await startWorkflow(harness, projectId, 'topup-flow', `Top-up cancelled run v${version}`, undefined, true);
    await harness.rpc('WorkflowCancelItem', stepped.id);
    await awaitWorkflowState(harness, 'cancelled run', stepped.id, 'cancelled');
    return item.id;
  });

  await step('projects', async () => {
    await harness.rpc('SetProjectWorktreeSetup', projectId, { copy: ['README.md'], run: [['true']], timeout: '1m' });
    const archived = await seedWorkflowProject(harness, `topup-archived-v${version}-${nonce}`, [], [], '', { 'README.md': '# archived\n' });
    await harness.rpc('RenameProject', archived.projectId, `Top-up archived v${version}`);
    await harness.rpc('UpdateProjectSortPositions', [archived.projectId, projectId]);
    await harness.rpc('ArchiveProject', archived.projectId);
    return archived.projectId;
  });

  await step('trackers', () => connectTrackers(harness, projectId));

  threadIds.push(...await step('threadRequests', () => writeThreadRequests(harness, version, nonce)));
  threadIds.push(...(await step('peerSpawns', () => writePeerSpawns(harness, version, nonce))).threadIds);
  // After the trackers, so a Jira connection is on, and after turns ran.
  await step('activity', () => recordActivity(harness, threadIds[0], threadIds));

  // Workflow runs add a thread per phase beside the threads listed here,
  // so the thread counts are floors. The thread-request and peer-spawn
  // projects are new on every run.
  const growth: CensusGrowth = {
    exact: { projects: (project.created ? 1 : 0) + (unverified.created ? 1 : 0) + 2, archivedProjects: 1, pinned: 2, groups: 1 },
    atLeast: { threads: threadIds.length - 1, archived: 1 },
  };
  return { projectId, growth, threadIds, scenarioThreads, workItemId, ms };
}

/**
 * A screen's input on threadId, on the desktop and then on the phone, and
 * the days of the threads whose turns the steps since the trackers started,
 * as the activity recorder writes them at its flush (activity-flush in
 * HARNESS_TIMING), polled until this run's rows are in the database: the
 * two spans that start at the reports, and a day of one of this run's
 * threads today.
 */
async function recordActivity(harness: HarnessApp, threadId: string, threadIds: readonly string[]): Promise<void> {
  const since = Date.now();
  await harness.rpc('RecordActivity', threadId, false, 0);
  await harness.rpc('RecordActivity', threadId, true, 0);
  const { dbPath } = await harness.rpc<{ dbPath: string }>('HarnessInfo');
  const today = new Date(since);
  const day = `${today.getFullYear()}-${String(today.getMonth() + 1).padStart(2, '0')}-${String(today.getDate()).padStart(2, '0')}`;
  const ids = threadIds.map((id) => `'${id.replaceAll("'", "''")}'`).join(', ');
  const mine = `thread_id = '${threadId.replaceAll("'", "''")}' AND start_ms >= ${since}`;
  await expect.poll(async () => {
    const [written] = await dbQuery<{ desk: number; phone: number; days: number }>(dbPath,
      `SELECT (SELECT count(*) FROM activity_spans WHERE ${mine} AND phone = 0) AS desk,
              (SELECT count(*) FROM activity_spans WHERE ${mine} AND phone = 1) AS phone,
              (SELECT count(*) FROM thread_days WHERE day = '${day}' AND thread_id IN (${ids})) AS days`);
    return Math.min(written.desk, written.phone, written.days) > 0;
  }, { timeout: 30_000, message: "the recorder wrote this run's desktop span, phone span and thread day" }).toBe(true);
}

/**
 * The fake Jira's connection, checked and with its token's expiry, the
 * Tempo connection under it, a renamed Linear connection that is off, and
 * the fake Jira's two boards listed on the version's project, the first
 * opening first. One site and
 * account holds one connection, so a run after an earlier one reuses the
 * connections it made.
 */
async function connectTrackers(harness: HarnessApp, projectId: string): Promise<void> {
  await seedFakeJira(harness, {
    projects: [],
    boards: [{ id: 2, name: 'WEB kanban', type: 'kanban', project: 'WEB', columns: [{ name: 'To Do', statuses: ['10000'] }] }],
  });
  const connections = await harness.rpc<TrackerConnection[]>('TrackerConnections');
  // A connection's site is the https origin the app made of the one given.
  const jira = connections.find((c) => c.kind === 'jira' && c.site === `https://${JIRA_SITE}` && c.account === JIRA_ACCOUNT)
    ?? await harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'jira', site: JIRA_SITE, account: JIRA_ACCOUNT });
  // The token a person would paste into the key file the app named.
  await writeFile(jira.keyFilePath, JIRA_TOKEN, { mode: 0o600 });
  const verified = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', jira.id);
  if (verified.problem) throw new Error(`the fake Jira refused the connection: ${JSON.stringify(verified.problem)}`);
  await harness.rpc('SetTrackerConnectionTokenExpiry', jira.id, Date.now() + 90 * 24 * 3600_000);
  if (!connections.some((c) => c.parentId === jira.id)) {
    await harness.rpc('AddTrackerConnection', { kind: 'tempo', site: '', parentId: jira.id });
  }
  if (!connections.some((c) => c.kind === 'linear')) {
    const linear = await harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'linear', site: 'example' });
    await harness.rpc('RenameTrackerConnection', linear.id, 'Top-up Linear');
    await harness.rpc('SetTrackerConnectionEnabled', linear.id, false);
  }
  // Built from the list the project holds: a run after an earlier one at
  // this version reuses its project.
  const listed = await harness.rpc<Array<{ connection: string; board: string; opens_first: boolean }>>('ProjectTrackerBoards', projectId);
  const built = listed.map(({ connection, board, opens_first }) => ({ connection, board, opens_first }));
  await harness.rpc('SetProjectTrackerBoards', projectId, built, [
    { connection: jira.id, board: BOARD_ID, opens_first: true, name: 'WEB board', tracker_project: 'WEB' },
    { connection: jira.id, board: '2', opens_first: false, name: 'WEB kanban', tracker_project: 'WEB' },
  ]);

  // The scrum board read into the mirror as a client opening it reads it
  // (tracker-board-wire.spec.ts). A refresh returns once its read ran
  // (structure, delta, reconcile and history), so the rows the corpus
  // keeps are written by answers, never by a tick's timing. A refresh the
  // tracker refused also returns, over the rows an earlier run left, so
  // the board's sync state is what says this read ran and was answered.
  const sprint = [{ kind: 'sprint', sprint: SPRINT_ID }];
  const sub = await harness.rpc<{ id: string }>('SubscribeTrackerBoard', projectId, jira.id, BOARD_ID, sprint, true);
  const asked = Date.now();
  await harness.rpc('RefreshTrackerSubscription', sub.id);
  type Rows = { rows: Array<{ card: { key: string } }> };
  const view = await harness.rpc<{ name: string; sync: { read_at?: number; failure?: unknown }; sections: Rows[] }>('TrackerBoard', sub.id, sprint);
  const keys = (page: Rows) => page.rows.map((r) => r.card.key).sort();
  if (view.sync.failure !== undefined || (view.sync.read_at ?? 0) < asked) {
    throw new Error(`the fake Jira's board was not read: its sync state is ${JSON.stringify(view.sync)}`);
  }
  if (view.name !== 'WEB board' || keys(view.sections[0]).join() !== 'WEB-1,WEB-2') {
    throw new Error(`the fake Jira's board read as ${view.name} with sprint rows ${keys(view.sections[0]).join()}`);
  }
  const backlog = await harness.rpc<Rows>('TrackerSectionRows', sub.id, { kind: 'backlog' }, null);
  if (keys(backlog).join() !== 'WEB-3') throw new Error(`the fake Jira's backlog read as ${keys(backlog).join()}`);
  await harness.rpc('ReleaseTrackerSubscription', sub.id);
}

/** The chat-bar settings the top-up's draft threads carry, one thread each. */
const THREAD_VARIANTS: Array<{ title: string; provider?: 'claude' | 'codex'; model?: string; options: Record<string, unknown> }> = [
  { title: 'plan, read-only, high effort', options: { mode: 'plan', runtimeMode: 'read-only', reasoningEffort: 'high' } },
  { title: 'fast mode, low effort, compaction overrides', options: { fastMode: true, reasoningEffort: 'low', autoCompactStandardPercent: 70, autoCompactExtendedPercent: 85 } },
  { title: 'medium effort, auto-accept edits', options: { reasoningEffort: 'medium', runtimeMode: 'auto-accept-edits' } },
  { title: 'max effort, auto', options: { reasoningEffort: 'max', runtimeMode: 'auto' } },
  { title: 'codex ultra effort', provider: 'codex', model: ULTRA_CODEX_MODEL, options: { reasoningEffort: 'ultra' } },
  { title: 'codex worktree', provider: 'codex', options: { worktreeBranch: 'topup/worktree', worktreeBaseLocal: true } },
];

/** The top-up's project, seeded with its workflow definitions, or found from an earlier run at this version. */
/**
 * A project whose GitHub origin the forge does not have, so its identity
 * read fails and the row carries an identity error. A checkout with no
 * origin carries none: it is a plain folder.
 */
async function ensureUnverifiedProject(harness: HarnessApp, version: string): Promise<{ created: boolean }> {
  const name = `topup-unverified-v${version}`;
  const rows = (await harness.rpc<Array<{ project: { path: string } }> | null>('ListProjects')) ?? [];
  if (rows.some((row) => path.basename(row.project.path) === name)) return { created: false };
  const seeded = await harness.rpc<{ projects: Array<{ projectId: string; path: string }> }>('HarnessSeed', {
    projects: [{ name, repo: { commits: [{ message: 'init', files: { 'README.md': `# Unverified v${version}\n` } }] } }],
  });
  const [project] = seeded.projects;
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  execFileSync('git', ['remote', 'add', 'origin', `https://github.com/ao-corpus/${name}.git`], { cwd: project.path, env, stdio: 'ignore' });
  const refreshed = await harness.rpc<{ identityError?: string }>('RefreshProjectIdentity', project.projectId);
  expect(refreshed.identityError, 'an origin the forge does not have left no identity error').toBeTruthy();
  return { created: true };
}

async function ensureTopUpProject(harness: HarnessApp, version: string): Promise<{ projectId: string; created: boolean }> {
  const name = `topup-v${version}`;
  const rows = (await harness.rpc<Array<{ project: { id: string; path: string } }> | null>('ListProjects')) ?? [];
  const existing = rows.find((row) => path.basename(row.project.path) === name);
  if (existing) return { projectId: existing.project.id, created: false };
  const seeded = await seedWorkflowProject(
    harness,
    name,
    [
      { name: 'topup-flow', yaml: singlePhaseWorkflow('topup-flow', '        - to: done', 'write') },
      { name: 'topup-gate', yaml: humanGateWorkflow('topup-gate') },
    ],
    [],
    '',
    { 'README.md': `# Top-up v${version}\n` },
  );
  return { projectId: seeded.projectId, created: true };
}

async function uploadAttachment(harness: HarnessApp, threadId: string, name: string, mime: string, bytes: Buffer): Promise<string> {
  const ticket = await harness.rpc<string>('MintAttachmentUploadTicket', threadId, name, mime, bytes.length);
  const upload = await fetch(new URL(ticket, harness.url), { method: 'PUT', body: new Uint8Array(bytes) });
  if (upload.status !== 200) throw new Error(`${name}: attachment upload answered ${upload.status}: ${await upload.text()}`);
  return ((await upload.json()) as { id: string }).id;
}

async function awaitWorkflowState(harness: HarnessApp, label: string, itemId: string, state: string, reason?: string): Promise<void> {
  try {
    await harness.waitForEvent<{ itemId: string; to: string; reason?: string }>(
      'workflow:item-state',
      (event) => event.itemId === itemId && event.to === state && (reason === undefined || event.reason === reason),
      WORKFLOW_TIMEOUT_MS,
    );
  } catch (error) {
    const detail = await harness.rpc<{ item: { state: string; reason?: string } }>('WorkflowGetItem', itemId)
      .catch((cause: Error) => ({ item: { state: `unreadable (${cause.message})`, reason: undefined as string | undefined } }));
    throw new Error(`top-up ${label}: the run never reached ${state}${reason ? ` (${reason})` : ''}; ` +
      `it is ${detail.item.state}${detail.item.reason ? ` (${detail.item.reason})` : ''}: ${(error as Error).message}`);
  }
}

/** One live turn of `scenario` in a new thread; returns the thread id once the turn is at rest. */
async function runScenarioTurn(
  harness: HarnessApp,
  projectId: string,
  scenario: TopUpScenario,
  provider: 'claude' | 'codex',
  version: string,
): Promise<string> {
  await harness.rpc('HarnessSetScenario', { name: scenario.name });
  const thread = await harness.rpc<{ id: string }>('CreateThread', {
    projectId,
    title: `Top-up v${version}: ${scenario.name}${scenario.approve === 'deny' ? ' (denied)' : ''}`,
    provider,
    model: MODEL[provider],
    mode: 'chat',
  });
  await harness.startSession(thread.id);
  await harness.waitForEvent<MockEvent>('harness:mock', (ev) => ev.report.kind === 'registered');
  const approval = scenario.approve
    ? harness.waitForEvent<ApprovalEvent>('provider:approval',
      (ev) => ev.action === 'request' && ev.request?.threadId === thread.id, TURN_TIMEOUT_MS)
    : undefined;
  // A session that dies ends its turn when the backend handles the death,
  // after the frames before it: the mock's own exit report travels apart
  // from them and can arrive before the turn has opened.
  const ended = scenario.exits
    ? harness.waitForEvent<{ threadId?: string }>('provider:session_died',
      (ev) => ev.threadId === thread.id, TURN_TIMEOUT_MS)
    : harness.waitForEvent<{ threadId?: string }>('provider:turn_completed',
      (ev) => ev.threadId === thread.id, TURN_TIMEOUT_MS);
  await harness.send(thread.id, `Top-up turn for ${scenario.name}.`);
  if (approval) {
    const pending = await approval;
    await harness.rpc('RespondToApproval', thread.id, { requestId: pending.request!.requestId, decision: scenario.approve });
  }
  await ended;
  await expect.poll(
    async () => (await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', thread.id)).activeTurn,
    { message: `${scenario.name}: the turn did not come to rest`, timeout: TURN_TIMEOUT_MS },
  ).toBeFalsy();
  return thread.id;
}

/** What one thread still runs or owes (HarnessThreadsWorking). */
interface ThreadWork {
  threadId: string;
  activeTurn: boolean;
  pendingSends: boolean;
  waitingOn?: string;
  compacting: boolean;
}

/** The ids of the threads in `harness`'s store: one walk of the threads table's key, about a thousand rows at the release check's scale. */
export function storeThreads(harness: HarnessApp): Set<string> {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return new Set((database.prepare('SELECT id FROM threads').all() as Array<{ id: string }>).map((row) => row.id));
  } finally {
    database.close();
  }
}

/**
 * The open turn rows of the threads a JSON array names, through the
 * partial index of open turns (idx_turns_inflight).
 */
export const OPEN_TURNS_SQL = `SELECT thread_id, turn_index FROM turns
  WHERE thread_id IN (SELECT value FROM json_each(?)) AND completed_at IS NULL
  ORDER BY thread_id, turn_index`;

/**
 * The streaming and running rows of the threads a JSON array names, through
 * the partial index of unsettled rows (idx_items_unsettled), whose predicate
 * the status term repeats so the planner can use it. A background tool
 * call runs past its turn on purpose; a parked stop is history.
 */
export const UNSETTLED_ROWS_SQL = `SELECT thread_id, id, kind, status FROM items
  WHERE thread_id IN (SELECT value FROM json_each(?)) AND status IN ('running', 'streaming')
    AND NOT (kind = 'tool_call' AND is_background = 1)
  ORDER BY thread_id, turn_index, item_index`;

/**
 * assertAtRest fails unless no thread of the top-up's runs or owes a turn,
 * live or in the store: every thread of `harness`'s store but the
 * `inherited` ones, workflow phase threads and the helpers a tool spawned
 * included. The live half is the backend's own answer
 * (HarnessThreadsWorking): an open turn, a message queued or written with
 * its turn still to open, a prompt waiting, or a compaction. The durable
 * half is what the store says, which a reader of the store after the
 * top-up meets, the archive included: an open turn row, or a streaming or
 * running row. The two part when a settlement write fails, since triage
 * ends the live turn either way. Each step waits for the turns it starts
 * and runs this when it returns, so a step that returns mid-turn fails
 * here, naming the step and the thread. The loops, watches and runs the
 * top-up leaves armed or open on purpose are not turns. `step` names the
 * caller in the failure.
 */
export async function assertAtRest(harness: HarnessApp, step: string, inherited: Set<string>): Promise<void> {
  const own = [...storeThreads(harness)].filter((id) => !inherited.has(id));
  if (own.length === 0) return;
  const working = await harness.rpc<ThreadWork[]>('HarnessThreadsWorking', own);
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  const lines: string[] = [];
  try {
    const ids = JSON.stringify(own);
    const open = database.prepare(OPEN_TURNS_SQL).all(ids) as Array<{ thread_id: string; turn_index: number }>;
    const unsettled = database.prepare(UNSETTLED_ROWS_SQL).all(ids) as Array<{ thread_id: string; id: string; kind: string; status: string }>;
    if (working.length === 0 && open.length === 0 && unsettled.length === 0) return;
    const title = (threadId: string) =>
      (database.prepare('SELECT title FROM threads WHERE id = ?').get(threadId) as { title: string } | undefined)?.title ?? '(no row)';
    const name = (threadId: string) => `"${title(threadId)}" (${threadId})`;
    for (const work of working) {
      const what = [
        work.activeTurn ? 'a turn is open' : '',
        work.pendingSends ? 'a message is queued or its turn is still to open' : '',
        work.waitingOn ? `an ${work.waitingOn} waits` : '',
        work.compacting ? 'it is compacting' : '',
      ].filter(Boolean);
      lines.push(`  - ${name(work.threadId)}: ${what.join(', ')}`);
    }
    for (const row of open) lines.push(`  - ${name(row.thread_id)}: turn ${row.turn_index} is open in the store`);
    for (const row of unsettled) lines.push(`  - ${name(row.thread_id)}: ${row.kind} row ${row.id} is ${row.status} in the store`);
  } finally {
    database.close();
  }
  throw new Error(`top-up step ${step} returned while a turn of its own still runs or is owed, live or in the store; ` +
    `it waits for each turn it starts to end:\n${lines.join('\n')}`);
}
