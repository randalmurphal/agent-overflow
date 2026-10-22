// The top-up: the slice of use the candidate writes into the corpus at its
// own schema. The release check runs it after the migration so the corpus
// the next release inherits carries rows every release since the feature
// landed has written; the coverage gate runs it on a fresh store to prove
// it still writes what the schema can hold. The steps are fixed: one thread
// per library scenario in topUpScenarios, driven through the mock provider
// the way a user's turn runs, then the sidebar and workflow operations.
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import * as path from 'node:path';
import { expect } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { topUpScenarios, type TopUpScenario } from './upgrade-corpus-config.js';
import type { CensusGrowth } from './upgrade-corpus-helpers.js';
import { timed } from './upgrade-corpus-tools.js';
import { compactBoundaryLine } from './agent-visibility-helpers.js';
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
  const library = await harness.rpc<{ library: LibraryEntry[] }>('HarnessListScenarios');
  const providers = new Map(library.library.map((entry) => [entry.name, entry.provider]));

  // One project per candidate version, reused when a run at the same
  // version already made it (a re-run, or a pull request before the
  // version bump).
  const project = await timed(() => ensureTopUpProject(harness, version));
  ms.project = project.ms;
  const unverified = await timed(() => ensureUnverifiedProject(harness, version));
  ms.unverifiedProject = unverified.ms;
  const { projectId } = project.value;
  const nonce = randomBytes(3).toString('hex');

  const scenarioThreads: Record<string, string> = {};
  const threadIds: string[] = [];
  for (const scenario of topUpScenarios) {
    const provider = providers.get(scenario.name);
    if (!provider) throw new Error(`topUpScenarios names ${scenario.name}, which the library does not have`);
    const key = scenario.approve === 'deny' ? `${scenario.name}:deny` : scenario.name;
    const turn = await timed(() => runScenarioTurn(harness, projectId, scenario, provider, version));
    ms[`turn:${key}`] = turn.ms;
    scenarioThreads[key] = turn.value;
    threadIds.push(turn.value);
  }

  const sidebar = await timed(async () => {
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
    await harness.rpc('SendMessage', reverted, 'Second top-up question.', null);
    await second;
    const rows = await harness.rpc<Array<{ id: string; kind: string; summary: string }>>('ListItems', reverted, false);
    const target = rows.find((row) => row.kind === 'user_text' && row.summary === 'Second top-up question.');
    if (!target) throw new Error('codex-revert-paginated: the second user message is not in the timeline');
    const resent = harness.waitForEvent<{ threadId?: string }>('provider:turn_completed', (ev) => ev.threadId === reverted, TURN_TIMEOUT_MS);
    await harness.rpc('RevertConversationAndResendMessage', reverted, target.id, { content: 'Second top-up question, rephrased.' });
    await resent;

    await harness.rpc('PinThread', scenarioThreads['tool-call']);
    await harness.rpc('PinThread', scenarioThreads['file-edit']);
    await harness.rpc('SetThreadPinGroup', scenarioThreads['file-edit'], 1);
    const group = await harness.rpc<{ id: string }>('CreateThreadGroup', projectId, `Top-up v${version} ${nonce}`);
    await harness.rpc('SetThreadGroup', [scenarioThreads['codex-basic'], scenarioThreads['codex-approval']], group.id);
    await harness.rpc('ArchiveThread', scenarioThreads['session-death']);
    await harness.rpc('RenameThread', scenarioThreads['usage-limit-claude'], `Top-up v${version}: renamed after its usage limit`);
    await harness.rpc('SetChatBarFavorite', { kind: 'model', provider: 'claude', value: MODEL.claude, label: 'Opus' }, true);
    // Two assistants made by New Assistant, one serving every project and
    // told about every tick and one moved to a new thread by New Thread,
    // Same Assistant, which leaves a past thread. Each opening and handoff
    // message takes a turn, whose session keeps the receipt of its memory
    // delivery. A new assistant takes the provider of the thread created
    // last, so a Claude thread goes first.
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
    const told = await assistant(`Top-up v${version} ${nonce}: told every tick`, true);
    await harness.rpc('SetStandingAgentNotifyOnTick', told, true);
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
    // its own. The hourly watch is run once now and keeps its stderr; a
    // short watch fires once, and the turn its wake starts cancels it
    // before a second fire is due; a long-running watch is started and
    // stopped. Nothing is left running for a later boot of the corpus to
    // restart.
    const agent = { id: await assistant(`Top-up v${version} ${nonce}: loop and watch`) };
    const record = await harness.rpc<{ home: string }>('GetStandingAgent', agent.id);
    const home = record.home;
    // The scenario below applies to the session it starts.
    await harness.rpc('StopSession', agent.id);
    await harness.rpc('HarnessSetScenario', {
      cwd: home,
      scenario: plainScenario({ name: 'topup-helper', provider: 'codex', texts: ['Collected the notes.'] }),
    });
    await harness.rpc('HarnessSetScenario', {
      scenario: threadToolsScenario({
        name: 'topup-wakes',
        provider: 'claude',
        turns: [{
          steps: [
            { call: { tool: 'thread_spawn', args: { prompt: 'Collect the inbox notes.', title: `Top-up v${version}: helper`, provider: 'codex', wait_seconds: 0 } } },
            { call: { tool: 'thread_loop', args: { label: 'board', note: 'look at the board', every_seconds: 3600 } } },
            // A run prints nothing on stdout, so it wakes nobody, and the
            // line on stderr is kept on the row (last_stderr).
            { call: { tool: 'thread_watch', args: { label: 'inbox', command: 'echo "inbox: nothing new" >&2', cwd: home, every_seconds: 3600, timeout_seconds: 30 } } },
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
          // The turn compacts first, so the corpus holds a compaction row
          // and the receipt its hook keeps.
          steps: [{ emitLines: [compactBoundaryLine('topup-compaction-${TURN}')] }, { call: { tool: 'thread_cancel', args: { label: 'once' } } }],
          text: 'Stopped the one-off watch.',
        }],
        afterTurns: 'silent',
      }),
    });
    await harness.rpc('StartSession', agent.id);
    const scheduled = awaitTurnCompleted(harness, agent.id, TURN_TIMEOUT_MS);
    await harness.rpc('SendMessage', agent.id, 'Look at the board and the inbox every hour.', null);
    const spawned = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', timeoutMs: TURN_TIMEOUT_MS });
    if (spawned.isError || !spawned.value) throw new Error(`top-up thread_spawn of a helper was refused: ${spawned.text}`);
    await awaitTurnCompleted(harness, spawned.value.thread_id, TURN_TIMEOUT_MS);
    threadIds.push(spawned.value.thread_id);
    for (const label of ['inbox', 'once', 'tail', 'tail stopped']) {
      const answer = await awaitToolAnswer(harness, { tool: 'thread_watch', timeoutMs: TURN_TIMEOUT_MS });
      if (answer.isError) throw new Error(`top-up thread_watch ${label} was refused: ${answer.text}`);
    }
    await scheduled;
    // The short watch's one fire is waited for by its effect: the turn its
    // wake starts cancels the watch.
    const cancelled = await awaitToolAnswer(harness, { tool: 'thread_cancel', timeoutMs: TURN_TIMEOUT_MS });
    if (cancelled.isError) throw new Error(`top-up thread_cancel was refused: ${cancelled.text}`);
    await awaitTurnCompleted(harness, agent.id, TURN_TIMEOUT_MS);
    // A cancel keeps nothing the row carried, so the hourly watch, run once
    // now, is the row that keeps a stderr tail. It stays armed an hour out.
    type WakeRow = { id: string; label: string; state: string; lastRunAt?: number };
    const inbox = async () =>
      (await harness.rpc<{ rows: WakeRow[] }>('ListThreadWakes', agent.id)).rows.find((row) => row.label === 'inbox');
    const armed = await inbox();
    if (!armed) throw new Error('top-up watch inbox is not listed');
    await harness.rpc('WakeThreadWakeNow', agent.id, 'watch', armed.id);
    await expect
      .poll(async () => {
        const row = await inbox();
        return row?.state === 'armed' && (row.lastRunAt ?? 0) > 0;
      }, { timeout: TURN_TIMEOUT_MS })
      .toBe(true);

    // Threads at every chat-bar setting the schema enumerates, drafts only.
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
  ms.sidebar = sidebar.ms;


  const workflow = await timed(async () => {
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
  ms.workflow = workflow.ms;

  const projects = await timed(async () => {
    await harness.rpc('SetProjectWorktreeSetup', projectId, { copy: ['README.md'], run: [['true']], timeout: '1m' });
    const archived = await seedWorkflowProject(harness, `topup-archived-v${version}-${nonce}`, [], [], '', { 'README.md': '# archived\n' });
    await harness.rpc('RenameProject', archived.projectId, `Top-up archived v${version}`);
    await harness.rpc('UpdateProjectSortPositions', [archived.projectId, projectId]);
    await harness.rpc('ArchiveProject', archived.projectId);
    return archived.projectId;
  });
  ms.projects = projects.ms;

  // Workflow runs add a thread per phase beside the threads listed here,
  // so the thread counts are floors.
  const growth: CensusGrowth = {
    exact: { projects: (project.value.created ? 1 : 0) + (unverified.value.created ? 1 : 0), archivedProjects: 1, pinned: 2, groups: 1 },
    atLeast: { threads: threadIds.length - 1, archived: 1 },
  };
  return { projectId, growth, threadIds, scenarioThreads, workItemId: workflow.value, ms };
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
  await harness.rpc('StartSession', thread.id);
  const registered = await harness.waitForEvent<MockEvent>('harness:mock', (ev) => ev.report.kind === 'registered');
  const approval = scenario.approve
    ? harness.waitForEvent<ApprovalEvent>('provider:approval',
      (ev) => ev.action === 'request' && ev.request?.threadId === thread.id, TURN_TIMEOUT_MS)
    : undefined;
  const ended = scenario.exits
    ? harness.waitForEvent<MockEvent>('harness:mock',
      (ev) => ev.report.kind === 'exiting' && ev.mockId === registered.mockId, TURN_TIMEOUT_MS)
    : harness.waitForEvent<{ threadId?: string }>('provider:turn_completed',
      (ev) => ev.threadId === thread.id, TURN_TIMEOUT_MS);
  await harness.rpc('SendMessage', thread.id, `Top-up turn for ${scenario.name}.`, null);
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
