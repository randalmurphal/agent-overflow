// The workflow tools (docs/specs/agent-workflow-tools.md, D77) driven by mock
// provider sessions that make REAL MCP calls against the app's
// `ao-workflow-tools` server: an interactive Claude or Codex thread starts a
// run, waits for it and reads it, and a workflow phase on either provider
// works inside the grants its definition declared. Everything past the mock's
// HTTP call is production code: session registration, the capability URL,
// tool scoping, and the bound workflow methods.
//
// No mock speaks claude-tui, so the claude-tui leg is proven at the argv level
// and at the app level through the real threadmcp transport, and the only
// unproven step is the real Claude TUI binary reading --mcp-config and
// --allowedTools, which the manual provider smoke covers.

import { test, expect } from './fixtures.js';
import type { ProviderName } from './thread-tools-helpers.js';
import {
  ALL_WORKFLOW_TOOLS,
  captureRunFromThread,
  START_RUN_PATTERN,
  answerPhasesDone,
  awaitWorkflowAnswer,
  doneFlowYaml,
  MODELS,
  otherProvider,
  seedToolsProject,
  setPhaseScenario,
  startAndWatchFromThread,
  type StartAnswer,
} from './workflows-agent-tools-helpers.js';
import { start, waitForWorkflowState, type WorkflowDetail } from './workflows-helpers.js';

const PROVIDERS: ProviderName[] = ['claude', 'codex'];

for (const provider of PROVIDERS) {
  test(`${provider}: an interactive thread starts a run, waits for it to rest and reads it through the workflow tools`, async ({
    harness,
    page,
  }) => {
    const phaseProvider = otherProvider(provider);
    await answerPhasesDone(harness, phaseProvider);
    const project = await seedToolsProject(
      harness,
      `workflow-tools-${provider}`,
      [{ name: 'importer', yaml: doneFlowYaml('importer', phaseProvider) }],
      { title: 'Workflow driver', provider },
    );

    const { tools, instructions, start: started, output } = await startAndWatchFromThread(
      harness,
      provider,
      project,
      'importer',
    );

    // An interactive thread in a project holds every tool, and its guide
    // is the one the server built for it.
    expect(tools).toEqual(ALL_WORKFLOW_TOOLS);
    expect(instructions).toContain('`run_start`');

    // D17: the run is bound to the thread that started it, and the wait saw
    // it rest without the thread polling.
    expect(started.start.workflowId).toBe('importer');
    expect(started.start.workflowScope).toBe('project');
    expect(started.start.boundThreadId).toBe(project.threadId);
    expect(started.start.skipped ?? false).toBe(false);
    expect(started.outcome).toBe('resting');
    expect(started.run).toMatchObject({ itemId: started.start.itemId, state: 'done', resting: true });
    expect(started.notify).toBeUndefined();
    expect(output).toMatchObject({ itemId: started.start.itemId, outputs: { verdict: true }, artifacts: [] });

    // The bound run's rest reaches the thread as a wake, which is the
    // notify the tool result promises a bound caller.
    const wake = await harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
      'harness:mock',
      (event) =>
        event.cwd === project.path &&
        event.report.kind === 'user_input' &&
        (event.report.input ?? '').includes(started.start.itemId),
      60_000,
    );
    expect(wake.report.input).toContain('done');

    await captureRunFromThread(harness, page, 'Workflow driver', started.start.itemId, `workflow-tools-${provider}-desktop.png`);
  });
}

// A read-only phase that holds start-run and introspect: the tools it sees
// are its grants plus the grant-free reads, and everything else is refused by
// name with the reason, never narrowed.
for (const provider of PROVIDERS) {
  test(`${provider}: a read-only phase works inside its grants, and the rest is refused by name`, async ({ harness }) => {
    const childProvider = otherProvider(provider);
    await answerPhasesDone(harness, childProvider);
    await setPhaseScenario(
      harness,
      provider,
      `granted-${provider}`,
      [
        { list: true },
        { call: 'run_start', args: { workflow_id: 'child-flow', seeds: { goal: 'Started by a phase' } } },
        { capture: 'CHILD', pattern: START_RUN_PATTERN },
        { call: 'run_start', args: { workflow_id: 'child-flow', seeds: { goal: 'Started by a phase' } } },
        { call: 'run_list', args: {} },
        { capture: 'PARENT', pattern: '"itemId":"([^"]+)","workflowId":"granting-flow"' },
        { call: 'run_control', args: { run_id: '${PARENT}', action: 'cancel' } },
        { call: 'run_resolve', args: { run_id: '${CHILD}', decision: 'approve' } },
        { call: 'memory_add', args: { kind: 'learning', text: 'The child run starts once.' } },
        { call: 'workflow_new', args: { starter: 'blank', id: 'from-a-phase', scope: 'project' } },
        { call: 'workflow_list', args: {} },
      ],
      { complete: true },
    );
    const project = await seedToolsProject(harness, `workflow-tools-grants-${provider}`, [
      { name: 'granting-flow', yaml: grantingFlowYaml(provider) },
      { name: 'child-flow', yaml: doneFlowYaml('child-flow', childProvider) },
    ]);

    const parent = await start(harness, project.projectId, 'granting-flow');

    const listing = await harness.awaitMcpTools({ server: 'ao-workflow-tools', timeoutMs: 60_000 });
    expect(listing.isError, listing.error).toBe(false);
    expect(listing.tools).toEqual([
      'workflow_list',
      'workflow_validate',
      'workflow_schema',
      'run_start',
      'run_status',
      'run_watch',
      'run_output',
      'run_inspect',
      'run_narrative',
      'run_list',
      'run_control',
      'run_amend',
      'run_guide',
      'memory_list',
    ]);
    // The guide names only what the phase holds, and points its memory at
    // the envelope it always had.
    expect(listing.instructions).not.toContain('run_resolve');
    expect(listing.instructions).not.toContain('`memory_add`');
    expect(listing.instructions).toContain("envelope's `memory` field");

    // A read-only phase's start-run grant takes effect, and the run it
    // starts binds to nothing.
    const first = await awaitWorkflowAnswer<StartAnswer>(harness, 'run_start');
    expect(first.isError, first.text).toBe(false);
    expect(first.value!.start).toMatchObject({ workflowId: 'child-flow', workflowScope: 'project' });
    expect(first.value!.start.skipped ?? false).toBe(false);
    expect(first.value!.start.boundThreadId ?? '').toBe('');

    // Surface-and-skip: the same call from the same phase answers with the
    // first run and starts nothing.
    const replay = await awaitWorkflowAnswer<StartAnswer>(harness, 'run_start');
    expect(replay.isError, replay.text).toBe(false);
    expect(replay.value!.start).toMatchObject({ itemId: first.value!.start.itemId, skipped: true });

    const listed = await awaitWorkflowAnswer<Array<{ itemId: string; workflowId: string }>>(harness, 'run_list');
    expect(listed.isError, listed.text).toBe(false);
    expect(listed.value!.map((run) => run.itemId)).toContain(parent.id);
    expect(listed.value!.filter((run) => run.workflowId === 'child-flow')).toHaveLength(1);

    // Row-level authority is the bound method's: a phase acts only on the
    // runs it started, not on the run it is a phase of.
    const cancelOwn = await awaitWorkflowAnswer(harness, 'run_control');
    expect(cancelOwn.isError).toBe(true);
    expect(cancelOwn.text).toContain('may only act on the runs it started');

    // A tool the grants do not admit is refused with the grant it needs.
    const resolved = await awaitWorkflowAnswer(harness, 'run_resolve');
    expect(resolved.isError).toBe(true);
    expect(resolved.text).toContain('[workflow_grant_required]');
    expect(resolved.text).toContain('"resolve"');

    // A read-only phase keeps the memory channel it had, whatever it was
    // granted: the two tools that write are refused for its access level.
    for (const tool of ['memory_add', 'workflow_new']) {
      const refused = await awaitWorkflowAnswer(harness, tool);
      expect(refused.isError, tool).toBe(true);
      expect(refused.text).toContain('[workflow_read_only]');
      expect(refused.text).toContain('read-only');
    }
    const workflows = await awaitWorkflowAnswer<{ workflows: Array<{ id: string }> }>(harness, 'workflow_list');
    expect(workflows.value!.workflows.map((entry) => entry.id).sort()).toEqual(['child-flow', 'granting-flow']);

    await waitForWorkflowState(harness, first.value!.start.itemId, 'done');
    await waitForWorkflowState(harness, parent.id, 'done');
  });
}

test('a write phase records campaign memory and scaffolds a definition without a grant', async ({ harness }) => {
  await setPhaseScenario(
    harness,
    'claude',
    'write-phase',
    [
      { call: 'memory_add', args: { kind: 'learning', text: 'The importer needs the schema first.' } },
      { call: 'memory_list', args: {} },
      { call: 'workflow_new', args: { starter: 'blank', id: 'from-a-phase', scope: 'project' } },
      { call: 'run_start', args: { workflow_id: 'writer-flow' } },
    ],
    { complete: true },
  );
  const project = await seedToolsProject(harness, 'workflow-tools-write', [
    { name: 'writer-flow', yaml: writerFlowYaml },
  ]);
  const item = await start(harness, project.projectId, 'writer-flow');

  const added = await awaitWorkflowAnswer<{ itemId: string; rootId: string; kind: string }>(harness, 'memory_add');
  expect(added.isError, added.text).toBe(false);
  expect(added.value).toMatchObject({ itemId: item.id, rootId: item.id, kind: 'learning' });
  const notes = await awaitWorkflowAnswer<{ notes: Array<{ text: string }>; total: number }>(harness, 'memory_list');
  expect(notes.value!.total).toBe(1);
  expect(notes.value!.notes[0]?.text).toBe('The importer needs the schema first.');
  const scaffolded = await awaitWorkflowAnswer<{ created: string[] }>(harness, 'workflow_new');
  expect(scaffolded.isError, scaffolded.text).toBe(false);
  expect(scaffolded.value!.created.some((path) => path.endsWith('from-a-phase.yaml'))).toBe(true);
  // Without start-run the phase cannot start work.
  const refused = await awaitWorkflowAnswer(harness, 'run_start');
  expect(refused.isError).toBe(true);
  expect(refused.text).toContain('[workflow_grant_required]');
  expect(refused.text).toContain('"start-run"');

  await waitForWorkflowState(harness, item.id, 'done');
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases[0]?.threadId).toBeTruthy();
});

function grantingFlowYaml(provider: ProviderName): string {
  return `id: granting-flow
name: granting-flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: work
    driver: agent
    provider: ${provider}
    model: ${MODELS[provider]}
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
}

const writerFlowYaml = `id: writer-flow
name: writer-flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: write
    driver: agent
    provider: claude
    model: ${MODELS.claude}
    prompt: writer-flow.md
    access: write
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
