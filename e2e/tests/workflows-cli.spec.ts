import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import * as nodePath from 'node:path';

import { test, expect, type HarnessMockEvent } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  doneResult,
  questionResult,
  seedWorkflowProject,
  setClaudeScenario,
  start,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

// The CLI execution surface (spec §5, D15/D17, D30) driven by the REAL binary
// against the REAL backend with the REAL credential.
//
// The CLI is the app binary itself, dispatched by verb: the same
// `bin/agent-overflow` the harness backend runs is what a session invokes as
// `agent-overflow run start …`. There is no second binary, so this spec proves
// the verb dispatch as well as everything behind it.
//
// The mock provider has no exec step, so it cannot shell out the way a live
// agent would. Instead each spec reads the AO_* environment the app injected
// into a live session (`HarnessSessionEnv` — a read of the token registry,
// never a mint) and spawns the binary with exactly that env. Everything
// downstream of the process boundary is production code: the entry dispatch,
// the CLI's own flag parsing, its HTTP POST to the scoped route, token
// authentication, grant authorization, and the bound methods.

const repoRoot = nodePath.resolve(import.meta.dirname, '..', '..');
const appBinary =
  process.env.AO_HARNESS_BIN ?? nodePath.join(repoRoot, 'bin', 'agent-overflow');

interface CommandResult {
  code: number;
  stdout: string;
  stderr: string;
}

// spawnCLI runs one command and collects its exit code and streams. The
// environment is deliberately minimal — the session variables under test plus
// what any process needs to run — because inheriting the suite's environment
// wholesale would let an ambient AO_* leak decide the outcome of a spec about
// AO_*. `path` is the PATH the child gets, which is also the lookup PATH Node
// uses to resolve a bare command name.
async function spawnCLI(
  command: string,
  path: string,
  sessionEnv: Record<string, string>,
  args: string[],
): Promise<CommandResult> {
  return await new Promise<CommandResult>((resolve, reject) => {
    const child = spawn(command, args, {
      env: { PATH: path, HOME: process.env.HOME ?? '', ...sessionEnv },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => (stdout += String(chunk)));
    child.stderr.on('data', (chunk) => (stderr += String(chunk)));
    child.on('error', reject);
    child.on('close', (code) => resolve({ code: code ?? -1, stdout, stderr }));
  });
}

// runAO invokes the harness-built binary by absolute path — what every case
// below uses, so a spec failure is about the CLI and not about where it lives.
async function runAO(
  sessionEnv: Record<string, string>,
  ...args: string[]
): Promise<CommandResult> {
  if (!existsSync(appBinary)) {
    throw new Error(`${appBinary} is missing; run \`make harness-build\` (make e2e does this for you)`);
  }
  return await spawnCLI(appBinary, process.env.PATH ?? '', sessionEnv, args);
}

// runPublishedAO resolves the command the way a provider session does: by bare
// name, through the single directory the app publishes under its config root
// (D30) and prepends to every session's PATH. PATH is *only* that directory,
// so a pass means the canonical-name link exists, points at a working binary,
// and dispatches the verb — not that some other agent-overflow was on PATH.
async function runPublishedAO(
  harness: HarnessApp,
  sessionEnv: Record<string, string>,
  ...args: string[]
): Promise<CommandResult> {
  return await spawnCLI(
    'agent-overflow',
    nodePath.join(harness.bootstrap.dataDir, 'bin'),
    sessionEnv,
    args,
  );
}

function parseJSON<T>(result: CommandResult): T {
  try {
    return JSON.parse(result.stdout) as T;
  } catch (err) {
    throw new Error(
      `agent-overflow did not print JSON (${err})\nstdout: ${result.stdout}\nstderr: ${result.stderr}`,
    );
  }
}

interface StartDocument {
  itemId: string;
  workflowId: string;
  workflowScope: string;
  state: string;
  skipped?: boolean;
  boundThreadId?: string;
  bindingWarning?: string;
}

interface RunDocument {
  itemId: string;
  workflowId: string;
  state: string;
  resting: boolean;
}

// sessionEnv reads the live session's injected environment. A thread with no
// session answers with an empty object, which is what a spawned process would
// see, so the helper insists on a credential rather than running the CLI against
// an env the app never issued.
async function sessionEnv(harness: HarnessApp, threadId: string): Promise<Record<string, string>> {
  const env = await harness.rpc<Record<string, string>>('HarnessSessionEnv', threadId);
  if (!env.AO_ENDPOINT || !env.AO_TOKEN) {
    throw new Error(`thread ${threadId} has no scoped credential`);
  }
  return env;
}

// A workflow whose single phase reports done, with a workflow-level output so
// `agent-overflow run output` has something to print.
function cliFlowYaml(id: string): string {
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
outputs:
  verdict:
    from: run.complete
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
        - to: done
cleanup: manual
`;
}

// The granting workflow parks on a question, which is what keeps its phase
// session — and therefore its scoped credential — alive while the spec drives
// the CLI as that phase.
const grantingFlowYaml = `id: granting-flow
name: granting-flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: work
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: granting-flow.md
    access: read-only
    grants:
      - start-run
      - introspect
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
        - to: done
cleanup: manual
`;

test('an interactive session starts, waits on, and reads a run through the app binary', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'cli-interactive', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(harness, 'workflow-cli-project', [
    { name: 'cli-flow', yaml: cliFlowYaml('cli-flow') },
  ]);
  const thread = await harness.rpc<{ id: string }>('CreateThread', {
    projectId: project.projectId,
    title: 'CLI driver',
    provider: 'claude',
    model: 'claude-opus-4-7',
    mode: 'chat',
  });
  await harness.rpc('StartSession', thread.id);
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (event) => event.report.kind === 'registered',
  );

  const env = await sessionEnv(harness, thread.id);
  expect(env.AO_THREAD_ID).toBe(thread.id);
  expect(env.AO_ENDPOINT).toMatch(/^http:\/\/127\.0\.0\.1:\d+$/);
  // A conversation is not inside a run: the phase half of the contract is
  // absent, which is how the CLI knows it is interactive.
  expect(env.AO_RUN_ID).toBeUndefined();
  expect(env.AO_PHASE_ID).toBeUndefined();

  // D30: the command resolves by bare name through the directory the app puts
  // on a session's PATH, with no runs started yet.
  const byName = await runPublishedAO(harness, env, 'run', 'list', '--json');
  expect(byName.code, byName.stderr).toBe(0);
  expect(parseJSON<RunDocument[]>(byName)).toEqual([]);

  const started = await runAO(env, 'run', 'start', 'cli-flow', '--seed', 'goal=Ship the CLI', '--json');
  expect(started.code, started.stderr).toBe(0);
  const startDoc = parseJSON<StartDocument>(started);
  expect(startDoc.workflowId).toBe('cli-flow');
  expect(startDoc.workflowScope).toBe('project');
  // D17: a run an interactive thread asked for binds to that thread.
  expect(startDoc.boundThreadId).toBe(thread.id);
  expect(startDoc.bindingWarning ?? '').toBe('');
  expect(startDoc.skipped ?? false).toBe(false);

  const waited = await runAO(env, 'run', 'wait', startDoc.itemId, '--json');
  expect(waited.code, waited.stderr).toBe(0);
  const restDoc = parseJSON<RunDocument>(waited);
  expect(restDoc.itemId).toBe(startDoc.itemId);
  expect(restDoc.state).toBe('done');
  expect(restDoc.resting).toBe(true);

  const outputs = await runAO(env, 'run', 'output', startDoc.itemId, '--json');
  expect(outputs.code, outputs.stderr).toBe(0);
  const outputDoc = parseJSON<{ outputs: Record<string, unknown>; artifacts: string[] }>(outputs);
  expect(outputDoc.outputs).toEqual({ verdict: true });
  expect(outputDoc.artifacts).toEqual([]);

  // `--wait` in one shot, human output: the start line prints before the wait so
  // a caller that loses patience still has the run id, then the resting line.
  const oneShot = await runAO(
    env, 'run', 'start', 'cli-flow', '--goal', 'Second pass', '--seed', 'goal=Second pass', '--wait');
  expect(oneShot.code, oneShot.stderr).toBe(0);
  const lines = oneShot.stdout.trim().split('\n');
  expect(lines).toHaveLength(2);
  expect(lines[0]).toContain('workflow=cli-flow');
  expect(lines[1]).toContain('state=done');
  const secondItemId = lines[1]?.split(' ')[0]?.replace('run=', '') ?? '';
  expect(secondItemId).not.toBe(startDoc.itemId);

  const listed = await runAO(env, 'run', 'list', '--json');
  expect(listed.code, listed.stderr).toBe(0);
  const runs = parseJSON<RunDocument[]>(listed);
  expect(runs.map((run) => run.itemId).sort()).toEqual([startDoc.itemId, secondItemId].sort());
  // An interactive scope replays freely: two identical starts are two runs,
  // because a human approved each invocation. Only a phase de-duplicates.
  expect(runs.every((run) => run.workflowId === 'cli-flow')).toBe(true);

  // The credential lives exactly as long as the session that holds it.
  await harness.rpc('StopSession', thread.id);
  await expect
    .poll(async () => Object.keys(await harness.rpc<Record<string, string>>('HarnessSessionEnv', thread.id)).length)
    .toBe(0);
  const revoked = await runAO(env, 'run', 'list', '--json');
  expect(revoked.code).toBe(2);
  expect(revoked.stderr).toContain('no longer valid');
});

test('a granted phase starts a run once and a re-entered call surfaces the prior start', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'cli-granting', [
    { steps: [{ emit: { lines: [questionResult('Should I start the child?')] } }] },
  ]);
  const project = await seedWorkflowProject(harness, 'workflow-cli-grants-project', [
    { name: 'granting-flow', yaml: grantingFlowYaml },
    { name: 'child-flow', yaml: cliFlowYaml('child-flow') },
  ]);

  const item = await start(harness, project.projectId, 'granting-flow');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'question');
  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  const phaseThreadId = parked.phases[0]?.threadId ?? '';
  expect(phaseThreadId).toBeTruthy();

  const env = await sessionEnv(harness, phaseThreadId);
  expect(env.AO_THREAD_ID).toBe(phaseThreadId);
  expect(env.AO_RUN_ID).toBe(item.id);
  expect(env.AO_PHASE_ID).toBe('work');

  // Every mock that registers from here on answers done, so the child run the
  // phase starts completes instead of parking like its parent.
  await setClaudeScenario(harness, 'cli-granted-child', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);

  const startArgs = ['run', 'start', 'child-flow', '--seed', 'goal=Started by a granted phase', '--json'];
  const started = await runAO(env, ...startArgs);
  expect(started.code, started.stderr).toBe(0);
  const startDoc = parseJSON<StartDocument>(started);
  expect(startDoc.workflowId).toBe('child-flow');
  expect(startDoc.skipped ?? false).toBe(false);
  // A phase-started run is decomposition, not conversation: it binds to nothing.
  expect(startDoc.boundThreadId ?? '').toBe('');
  expect(startDoc.bindingWarning ?? '').toBe('');
  await waitForWorkflowState(harness, startDoc.itemId, 'done');

  // Surface-and-skip: the identical call from the same phase does NOT fire a
  // second time. It answers with the original run and exits 0, because the
  // effect the caller asked for exists.
  const replayed = await runAO(env, ...startArgs);
  expect(replayed.code, replayed.stderr).toBe(0);
  const replayDoc = parseJSON<StartDocument>(replayed);
  expect(replayDoc.skipped).toBe(true);
  expect(replayDoc.itemId).toBe(startDoc.itemId);

  const replayedHuman = await runAO(env, ...startArgs.slice(0, -1));
  expect(replayedHuman.code, replayedHuman.stderr).toBe(0);
  expect(replayedHuman.stdout).toContain('skipped=true');

  // `introspect` is what makes a project-wide listing legible to a phase.
  const listed = await runAO(env, 'run', 'list', '--active', '--json');
  expect(listed.code, listed.stderr).toBe(0);
  const active = parseJSON<RunDocument[]>(listed);
  expect(active.map((run) => run.itemId)).toContain(item.id);
  // Exactly one child run exists: the replay above started nothing.
  const all = parseJSON<RunDocument[]>(await runAO(env, 'run', 'list', '--json'));
  expect(all.filter((run) => run.workflowId === 'child-flow')).toHaveLength(1);

  // Row-level scoping is not expressible as a method name: acting on a run this
  // phase did not start is refused by the bound method, including the run the
  // phase is itself part of.
  const cancelOwn = await runAO(env, 'run', 'cancel', item.id);
  expect(cancelOwn.code).toBe(2);
  expect(cancelOwn.stderr).toContain('may only act on the runs it started');

  // A method the phase's grants do not admit is a typed refusal that names the
  // missing grant, not a silent no-op.
  const scheduled = await runAO(env, 'schedule', 'child-flow', '--cron', '0 3 * * *');
  expect(scheduled.code).toBe(2);
  expect(scheduled.stderr).toContain('"schedule"');
  expect(scheduled.stderr).toContain("phase's grants");

  // A method outside the scoped allow-list does not exist for this token at all
  // — the surface stays unenumerable from a compromised agent session. the CLI has
  // no command for it, so this one goes over the same route by hand.
  const response = await fetch(`${env.AO_ENDPOINT}/rpc`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${env.AO_TOKEN}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ type: 'rpc', id: '1', method: 'ListThreads', params: [] }),
  });
  expect(response.status).toBe(200);
  const frame = (await response.json()) as { error?: { code: string; message: string } };
  expect(frame.error?.code).toBe('method_not_found');
});
