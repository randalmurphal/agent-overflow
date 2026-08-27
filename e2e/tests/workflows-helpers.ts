import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

export interface WorkflowItem {
  id: string;
  state: string;
  reason?: string;
}

export interface WorkflowPhase {
  phaseId: string;
  attempt: number;
  threadId?: string;
  outputEnvelope?: unknown;
  status: string;
}

// WorkflowUnit is one fan-out unit (or the join) of one phase attempt. Branch
// and worktree are populated for a unit the runner isolated; the join never has
// them, and a consumed unit's worktree is cleared once the join is done.
export interface WorkflowUnit {
  unitId: string;
  unitIndex: number;
  kind: string;
  status: string;
  threadId?: string;
  branch?: string;
  worktreePath?: string;
  unitAttempt: number;
}

// WorkflowRunMapRun is one run of the map's tree, narrowed to the linkage that
// identifies an invocation (spec §3a) — ONE name for one concept, root
// included. "Children" is not a kind of run, it is what `callChildren`'s filter
// produces from these, and typing the filter's output as its own interface said
// the root was something else.
//
// The wire omits a zero `parentAttempt` / `callDepth` (Go `omitempty`), so both
// are optional HERE and normalised to numbers by `callChildren` below — a spec
// comparing `child.callDepth` should be comparing numbers, not sometimes
// `undefined`.
export interface WorkflowRunMapRun {
  itemId: string;
  workflowId: string;
  state: string;
  reason?: string;
  parentItemId?: string;
  parentPhaseId?: string;
  // Empty for a phase call; names the fan-out unit that called this run
  // otherwise (§3a at unit scope).
  parentUnitId?: string;
  parentAttempt?: number;
  callDepth?: number;
}

// WorkflowRunMapRunLinkage is what callChildren hands back: the same row with
// the omitempty numbers filled in.
export interface WorkflowRunMapRunLinkage extends WorkflowRunMapRun {
  parentAttempt: number;
  callDepth: number;
}

// WorkflowRunMapRefusal is an answer the backend will never change its mind
// about (§4.2) — the RPC SUCCEEDS and carries this instead of a tree.
export interface WorkflowRunMapRefusal {
  code: string;
  message: string;
}

export interface WorkflowRunMap {
  rootItemId: string;
  runs: WorkflowRunMapRun[];
  refusal?: WorkflowRunMapRefusal;
}

// callChildren reads the runs one run CALLED, through the run map — the tree
// read that answers for call linkage. Any member resolves the same map, so this
// works for a root and for a run three waves down alike.
//
// It costs a WHOLE-TREE read every call: the RPC's answer is the root plus every
// descendant, with each run's frozen definition decoded server-side for its
// skeleton. That is cheap for the fixtures here and wrong to put in a poll —
// read it once and filter, rather than calling it per candidate parent.
//
// A REFUSAL throws rather than reading as "no children". `runs` is empty on a
// refused answer, so an id that names no run at all (a typo, a discarded run, a
// stale id) would otherwise satisfy `toHaveLength(0)` — the assertion most
// callers here make — while proving nothing.
export async function callChildren(
  harness: HarnessApp,
  itemId: string,
): Promise<WorkflowRunMapRunLinkage[]> {
  const map = await harness.rpc<WorkflowRunMap>('WorkflowGetRunMap', itemId);
  if (map.refusal) {
    throw new Error(
      `WorkflowGetRunMap(${itemId}) refused (${map.refusal.code}): ${map.refusal.message}`,
    );
  }
  return map.runs
    .filter((run) => run.parentItemId === itemId)
    .map((run) => ({ ...run, parentAttempt: run.parentAttempt ?? 0, callDepth: run.callDepth ?? 0 }));
}

export interface WorkflowDetail {
  item: WorkflowItem & {
    worktreePath?: string;
    branch?: string;
    baseBranch?: string;
    parentItemId?: string;
    parentPhaseId?: string;
    parentUnitId?: string;
    parentAttempt?: number;
    callDepth?: number;
    // The standing request to stop this tree at its next call boundary (D36).
    softStop?: boolean;
  };
  checkPhaseIds: string[];
  callPhaseIds?: string[];
  phases: WorkflowPhase[];
  units: WorkflowUnit[];
}

export interface WorkflowStateEvent {
  itemId: string;
  projectId: string;
  from: string;
  to: string;
  reason?: string;
}

export interface WorkflowProviderInput {
  mockId: string;
  protocol: 'claude' | 'codex';
  sessionRef: string;
  turn: number;
  input: string;
}

const prompt = 'Complete this workflow phase and return the required envelope.';

export interface WorkflowSeedDefinition {
  name: string;
  yaml: string;
  prompts?: Record<string, string>;
}

export interface WorkflowSeedItem {
  workflow: string;
  goal: string;
  seeds?: Record<string, unknown>;
  stepMode?: boolean;
  count?: number;
  target?: 'running' | 'needs-human' | 'done';
}

// repoFiles are committed into the seeded project's git repository. Tool-phase
// fixtures use it to ship the scripts their profile bindings name.
export async function seedWorkflowProject(
  harness: HarnessApp,
  projectName: string,
  definitions: WorkflowSeedDefinition[],
  items: WorkflowSeedItem[] = [],
  profile = '',
  repoFiles: Record<string, string> = {},
): Promise<SeedResult['projects'][number]> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: projectName,
        repo:
          Object.keys(repoFiles).length > 0
            ? { commits: [{ message: 'fixture scripts', files: repoFiles }] }
            : {},
        workflows: {
          definitions: definitions.map((definition) => ({
            ...definition,
            prompts: definition.prompts ?? {
              [`${definition.name}.md`]: prompt,
            },
          })),
          profile,
          items,
        },
      },
    ],
  });
  const project = seed.projects[0];
  if (!project) {
    throw new Error(`HarnessSeed returned no project for ${projectName}`);
  }
  return project;
}

export async function seedWorkflow(
  harness: HarnessApp,
  projectName: string,
  workflowName: string,
  yaml: string,
  profile = '',
): Promise<{ projectId: string; path: string }> {
  return await seedWorkflowProject(
    harness,
    projectName,
    [{ name: workflowName, yaml }],
    [],
    profile,
  );
}

export async function setClaudeScenario(
  harness: HarnessApp,
  name: string,
  turns: Array<{ label?: string; steps: unknown[] }>,
): Promise<void> {
  await harness.rpc('HarnessSetScenario', {
    scenario: { version: 1, name, provider: 'claude', turns },
  });
}

export async function start(
  harness: HarnessApp,
  projectId: string,
  workflowId: string,
  stepMode = false,
): Promise<WorkflowItem> {
  return await startWorkflow(
    harness,
    projectId,
    workflowId,
    `Run ${workflowId}`,
    undefined,
    stepMode,
  );
}

export async function startWorkflow(
  harness: HarnessApp,
  projectId: string,
  workflowId: string,
  goal: string,
  seeds: Record<string, unknown> = { goal },
  stepMode = false,
): Promise<WorkflowItem> {
  return await harness.rpc<WorkflowItem>(
    'WorkflowStartRun',
    projectId,
    workflowId,
    'project',
    goal,
    seeds,
    null,
    '',
    stepMode,
  );
}

export async function waitForWorkflowState(
  harness: HarnessApp,
  itemId: string,
  state: string,
  reason?: string,
): Promise<WorkflowStateEvent> {
  return await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) =>
      event.itemId === itemId &&
      event.to === state &&
      (reason === undefined || event.reason === reason),
  );
}

// waitForWorkflowProviderInput is the reusable workflow wire assertion: it
// returns both what AO sent and the provider session it targeted. Transcript
// rows and process ids cannot answer both questions, especially for Codex.
export async function waitForWorkflowProviderInput(
  harness: HarnessApp,
  protocol: 'claude' | 'codex',
): Promise<WorkflowProviderInput> {
  const event = await harness.waitForEvent<{
    mockId: string;
    protocol: string;
    report: { kind: string; turn?: number; input?: string; sessionRef?: string };
  }>(
    'harness:mock',
    (candidate) => candidate.protocol === protocol && candidate.report.kind === 'user_input',
  );
  const { input, sessionRef, turn } = event.report;
  if (!input || !sessionRef || !turn) {
    throw new Error(`incomplete ${protocol} user_input report: ${JSON.stringify(event)}`);
  }
  return { mockId: event.mockId, protocol, sessionRef, turn, input };
}

// setGlobalPause drives the one engine-level kill switch. While paused a
// started run is admitted and persisted `running` with its first phase held,
// so a spec can stage scenarios before any provider session exists.
export async function setGlobalPause(harness: HarnessApp, paused: boolean): Promise<void> {
  await harness.rpc('WorkflowSetGlobalPause', paused);
}

export async function waitForEnginePause(
  harness: HarnessApp,
  paused: boolean,
): Promise<void> {
  await harness.waitForEvent<{ paused: boolean }>(
    'workflow:engine-state',
    (event) => event.paused === paused,
  );
}

export function doneResult(outputs: Record<string, unknown>): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    structured_output: {
      status: 'done',
      outputs,
      question: null,
      reason: null,
    },
  });
}

// assistantText is one completed assistant message on the Claude wire. A
// read-only phase's narrative arrives this way and no other — its session cannot
// write files — so a scenario that emits one is what proves the runner recovers
// the narrative file the wake then points at.
export function assistantText(text: string): string {
  return JSON.stringify({
    type: 'assistant',
    message: {
      id: 'msg-narrative',
      role: 'assistant',
      content: [{ type: 'text', text }],
    },
  });
}

export function questionResult(question: string): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    structured_output: {
      status: 'question',
      outputs: null,
      question,
      reason: null,
    },
  });
}

export function questionEnvelope(question: string): Record<string, unknown> {
  return { status: 'question', outputs: null, question, reason: null };
}

export function singlePhaseWorkflow(
  id: string,
  gate: string,
  access: 'read-only' | 'write' = 'read-only',
  provider: 'claude' | 'codex' = 'claude',
): string {
  const model = provider === 'claude' ? 'claude-opus-4-7' : 'gpt-5.5';
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: ${provider}
    model: ${model}
    prompt: ${id}.md
    access: ${access}
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
${gate}
cleanup: manual
`;
}

// humanGateWorkflow is the three-phase shape the overlay spec parks on: `plan`
// runs, `review` finishes and asks a human, `apply` runs once the gate is
// approved. Three phases and not two because a reject loop must target a
// STRICT ancestor, and because the declared order is what makes the overlay's
// primary read `Approve → apply` instead of the generic fallback.
export function humanGateWorkflow(
  id: string,
  access: 'read-only' | 'write' = 'read-only',
): string {
  const phase = (phaseId: string, inputs: string, gate: string): string => `  - id: ${phaseId}
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: ${id}.md
    access: ${access}
    inputs:
${inputs}
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
${gate}
`;
  const boolInput = (name: string): string => `      ${name}:
        schema:
          type: boolean`;
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
${phase('plan', `      goal:
        schema:
          type: string`, '        - to: review')}${phase('review', boolInput('plan.complete'), `        - human:
            approve: apply
            reject:
              loop: plan
              max: 1`)}${phase('apply', boolInput('review.complete'), '        - to: done')}cleanup: manual
`;
}

export function retryableFailureWorkflow(id: string): string {
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: ${id}.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - when:
            eq:
              ref: run.complete
              value: false
          to: failed
        - to: done
`;
}

export function stuckResult(reason: string): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    structured_output: {
      status: 'stuck',
      outputs: null,
      question: null,
      reason,
    },
  });
}

// setCodexScenario stages the app-server JSON-RPC frames one Codex turn
// produces. `text` is the agent message the app reads the control envelope
// from, so a caller passes the envelope it wants that turn to answer with.
export async function setCodexScenario(
  harness: HarnessApp,
  name: string,
  envelope: Record<string, unknown>,
): Promise<void> {
  await setCodexTurns(harness, name, [envelope]);
}

// setCodexTurns is the Codex counterpart of setClaudeScenario. Keeping the
// multi-turn shape here lets workflow recovery tests use one provider-neutral
// driver instead of hand-authoring JSON-RPC frames per test.
export async function setCodexTurns(
  harness: HarnessApp,
  name: string,
  envelopes: Record<string, unknown>[],
  threadId = 'mock-codex-thread',
  responses: Record<string, string> = {},
): Promise<void> {
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name,
      provider: 'codex',
      codex: { threadId, responses },
      turns: envelopes.map((envelope) => {
        const text = JSON.stringify(JSON.stringify(envelope));
        return {
          steps: [
            {
              emit: {
                lines: [
                  '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}',
                  `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"agentMessage","id":"msg-\${TURN}","status":"completed","text":${text}}}}`,
                  '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}',
                ],
              },
            },
          ],
        };
      }),
      afterTurns: 'repeatLast',
    },
  });
}

export function doneEnvelope(outputs: Record<string, unknown>): Record<string, unknown> {
  return { status: 'done', outputs, question: null, reason: null };
}

export function stuckEnvelope(reason: string): Record<string, unknown> {
  return { status: 'stuck', outputs: null, question: null, reason };
}

export interface MockSessionConfig {
  permissionMode?: string;
  disallowedTools?: string[];
  sandbox?: string;
  approvalPolicy?: string;
  mcpServers?: string[];
}

export interface MockInfo {
  mockId: string;
  registration: { protocol: string; cwd: string };
  sessionConfig?: MockSessionConfig;
}

// mockSessions returns every mock of one protocol that has reported a launch
// config, in registration order, once at least `expected` have. Polling rather
// than event-waiting is deliberate: the config is observable before a session's
// first turn, so a listener started after the run begins can legitimately miss
// the event, while the latched value cannot be missed.
export async function mockSessions(
  harness: HarnessApp,
  protocol: string,
  expected: number,
): Promise<MockInfo[]> {
  const deadline = Date.now() + 15_000;
  let seen: MockInfo[] = [];
  while (Date.now() < deadline) {
    const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
    seen = mocks.filter(
      (mock) => mock.registration.protocol === protocol && mock.sessionConfig,
    );
    if (seen.length >= expected) return seen;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(
    `expected ${expected} ${protocol} session configs, saw ${seen.length}: ${JSON.stringify(seen)}`,
  );
}

export async function sessionConfigs(
  harness: HarnessApp,
  protocol: string,
  expected: number,
): Promise<MockSessionConfig[]> {
  const mocks = await mockSessions(harness, protocol, expected);
  return mocks.map((mock) => mock.sessionConfig as MockSessionConfig);
}
