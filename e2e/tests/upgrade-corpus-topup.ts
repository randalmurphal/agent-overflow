// The top-up: the slice of use the candidate writes into the corpus at its
// own schema. The release check runs it after the migration so the corpus
// the next release inherits carries rows every release since the feature
// landed has written; the coverage gate runs it on a fresh store to prove
// it still writes what the schema can hold. The steps are fixed: one thread
// per library scenario in topUpScenarios, driven through the mock provider
// the way a user's turn runs, then the sidebar and workflow operations.
import { randomBytes } from 'node:crypto';
import * as path from 'node:path';
import { expect } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { topUpScenarios, type TopUpScenario } from './upgrade-corpus-config.js';
import type { CensusGrowth } from './upgrade-corpus-helpers.js';
import { timed } from './upgrade-corpus-tools.js';
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
    // Two standing agents, one told about every tick.
    await harness.rpc('PromoteStandingAgent', scenarioThreads['tool-call']);
    await harness.rpc('PromoteStandingAgent', scenarioThreads['codex-basic']);
    await harness.rpc('SetStandingAgentNotifyOnTick', scenarioThreads['codex-basic'], true);

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

    // A gated run approved by a human, a run that asks a question, a run
    // cancelled while stepping, and a run an automation fired.
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

    await setClaudeScenario(harness, 'topup-workflow', done);
    const automation = await harness.rpc<{ id: string }>('WorkflowCreateAutomation', {
      projectId,
      workflowScope: 'project',
      enabled: true,
      workflowId: 'topup-flow',
      name: `Top-up nightly v${version}`,
      trigger: { kind: 'cron', expr: '0 3 * * *' },
      seeds: { goal: 'the top-up' },
    });
    const fired = await harness.rpc<{ id: string }>('WorkflowRunAutomationNow', automation.id);
    await awaitWorkflowState(harness, 'automation run', fired.id, 'done');
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
    exact: { projects: project.value.created ? 1 : 0, archivedProjects: 1, pinned: 2, groups: 1 },
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
