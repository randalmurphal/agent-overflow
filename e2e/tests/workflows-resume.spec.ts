import { test, expect } from './fixtures.js';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  doneEnvelope,
  doneResult,
  questionEnvelope,
  questionResult,
  seedWorkflowProject,
  setClaudeScenario,
  setCodexTurns,
  singlePhaseWorkflow,
  start,
  waitForWorkflowProviderInput,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

type Provider = 'claude' | 'codex';

const providers: Provider[] = ['claude', 'codex'];
const answer = 'Use option A';

async function setQuestionThenDoneScenario(
  harness: HarnessApp,
  provider: Provider,
  name: string,
  threadId: string,
): Promise<void> {
  if (provider === 'claude') {
    await setClaudeScenario(harness, name, [
      {
        label: 'question',
        steps: [{ emit: { lines: [questionResult('Which option?')] } }],
      },
      {
        label: 'done',
        steps: [{ emit: { lines: [doneResult({ complete: true })] } }],
      },
    ]);
    return;
  }
  await setCodexTurns(
    harness,
    name,
    [questionEnvelope('Which option?'), doneEnvelope({ complete: true })],
    threadId,
  );
}

async function setDoneScenario(
  harness: HarnessApp,
  provider: Provider,
  name: string,
  threadId: string,
): Promise<void> {
  if (provider === 'claude') {
    await setClaudeScenario(harness, name, [
      {
        label: 'done',
        steps: [{ emit: { lines: [doneResult({ complete: true })] } }],
      },
    ]);
    return;
  }
  await setCodexTurns(harness, name, [doneEnvelope({ complete: true })], threadId);
}

async function seedResumeWorkflow(
  harness: HarnessApp,
  provider: Provider,
  suffix: string,
): Promise<{ projectId: string; workflowId: string; sentinel: string }> {
  const workflowId = `${provider}-resume-${suffix}`;
  const sentinel = `ORIGINAL_TASK_${provider.toUpperCase()}_${suffix.toUpperCase()}`;
  const project = await seedWorkflowProject(harness, `${workflowId}-project`, [
    {
      name: workflowId,
      yaml: singlePhaseWorkflow(workflowId, '        - to: done', 'read-only', provider),
      prompts: { [`${workflowId}.md`]: `${sentinel}: finish the phase.` },
    },
  ]);
  return { projectId: project.projectId, workflowId, sentinel };
}

for (const provider of providers) {
  test(`${provider} workflow continuation sends only the resolution to the same provider session`, async ({
    harness,
  }) => {
    const fixture = await seedResumeWorkflow(harness, provider, 'same');
    await setQuestionThenDoneScenario(
      harness,
      provider,
      `${provider}-resume-same`,
      `${provider}-resume-same-session`,
    );

    const item = await start(harness, fixture.projectId, fixture.workflowId);
    const first = await waitForWorkflowProviderInput(harness, provider);
    expect(first.input).toContain(fixture.sentinel);
    await waitForWorkflowState(harness, item.id, 'needs-human', 'question');
    const before = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);

    await harness.rpc('WorkflowAnswerQuestion', item.id, answer);
    const resumed = await waitForWorkflowProviderInput(harness, provider);
    expect(resumed.sessionRef).toBe(first.sessionRef);
    expect(resumed.input).toContain('Resume the current workflow phase');
    expect(resumed.input).toContain(answer);
    expect(resumed.input).not.toContain(fixture.sentinel);
    expect(resumed.input).not.toContain('<workflow-system-instructions>');

    await waitForWorkflowState(harness, item.id, 'done');
    const after = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
    expect(after.phases).toHaveLength(2);
    expect(after.phases[1]?.threadId).toBe(before.phases[0]?.threadId);
  });

  test(`${provider} workflow with a lost cursor starts a new session with the full prompt`, async ({
    harness,
  }) => {
    const fixture = await seedResumeWorkflow(harness, provider, 'fresh');
    await setQuestionThenDoneScenario(
      harness,
      provider,
      `${provider}-resume-fresh-question`,
      `${provider}-resume-original-session`,
    );

    const item = await start(harness, fixture.projectId, fixture.workflowId);
    const first = await waitForWorkflowProviderInput(harness, provider);
    await waitForWorkflowState(harness, item.id, 'needs-human', 'question');
    const before = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
    const parkedThread = before.phases[0]?.threadId ?? '';
    expect(parkedThread).toBeTruthy();

    // Cursor loss only matters after the live process is gone. While it is
    // registered, that process is the authoritative continuation context even
    // if its durable cursor write has not landed yet.
    await harness.rpc('StopSession', parkedThread);
    await harness.rpc('HarnessClearThreadProviderCursor', parkedThread);
    await setDoneScenario(
      harness,
      provider,
      `${provider}-resume-fresh-done`,
      `${provider}-resume-replacement-session`,
    );
    await harness.rpc('WorkflowAnswerQuestion', item.id, answer);

    const restarted = await waitForWorkflowProviderInput(harness, provider);
    expect(restarted.sessionRef).not.toBe(first.sessionRef);
    expect(restarted.input).toContain(fixture.sentinel);
    expect(restarted.input).toContain('<workflow-system-instructions>');
    expect(restarted.input).toContain(answer);
    expect(restarted.input).not.toContain('Resume the current workflow phase');

    await waitForWorkflowState(harness, item.id, 'done');
    const after = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
    expect(after.phases.map((phase) => phase.status)).toEqual([
      'parked',
      'superseded',
      'completed',
    ]);
    expect(after.phases[2]?.threadId).not.toBe(parkedThread);
  });
}

test('Codex runtime cursor rejection supersedes the unsent continuation and restarts full', async ({
  harness,
}) => {
  const fixture = await seedResumeWorkflow(harness, 'codex', 'rejected');
  await setQuestionThenDoneScenario(
    harness,
    'codex',
    'codex-resume-rejected-question',
    'codex-resume-rejected-original',
  );

  const item = await start(harness, fixture.projectId, fixture.workflowId);
  const first = await waitForWorkflowProviderInput(harness, 'codex');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'question');
  const before = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  const parkedThread = before.phases[0]?.threadId ?? '';
  expect(parkedThread).toBeTruthy();
  await harness.rpc('StopSession', parkedThread);

  await setCodexTurns(
    harness,
    'codex-resume-rejected-done',
    [doneEnvelope({ complete: true })],
    'codex-resume-rejected-replacement',
    {
      'thread/resume':
        '{"jsonrpc":"2.0","id":${REQUEST_ID},"error":{"code":-32600,"message":"no rollout found for thread id ${THREAD_ID}"}}',
    },
  );
  await harness.rpc('WorkflowAnswerQuestion', item.id, answer);

  const restarted = await waitForWorkflowProviderInput(harness, 'codex');
  expect(restarted.sessionRef).not.toBe(first.sessionRef);
  expect(restarted.input).toContain(fixture.sentinel);
  expect(restarted.input).toContain(answer);
  expect(restarted.input).not.toContain('Resume the current workflow phase');

  await waitForWorkflowState(harness, item.id, 'done');
  const after = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(after.phases.map((phase) => phase.status)).toEqual(['parked', 'superseded', 'completed']);
  expect(after.phases[2]?.threadId).not.toBe(parkedThread);
});

for (const provider of providers) {
  test(`${provider} cold app restart preserves continuation context`, async () => {
    const dataDir = await mkdtemp(path.join(tmpdir(), `ao-workflow-restart-${provider}-`));
    let active: HarnessApp | undefined;
    try {
      active = await launchHarness({ dataDir });
      const fixture = await seedResumeWorkflow(active, provider, 'restart');
      await setQuestionThenDoneScenario(
        active,
        provider,
        `${provider}-resume-restart-question`,
        `${provider}-resume-restart-session`,
      );
      const item = await start(active, fixture.projectId, fixture.workflowId);
      const first = await waitForWorkflowProviderInput(active, provider);
      await waitForWorkflowState(active, item.id, 'needs-human', 'question');
      const before = await active.rpc<WorkflowDetail>('WorkflowGetItem', item.id);

      await active.stop();
      active = await launchHarness({ dataDir });
      await setDoneScenario(active, provider, `${provider}-resume-restart-done`, first.sessionRef);
      await active.rpc('WorkflowAnswerQuestion', item.id, answer);

      const resumed = await waitForWorkflowProviderInput(active, provider);
      expect(resumed.sessionRef).toBe(first.sessionRef);
      expect(resumed.input).toContain('Resume the current workflow phase');
      expect(resumed.input).toContain(answer);
      expect(resumed.input).not.toContain(fixture.sentinel);
      await waitForWorkflowState(active, item.id, 'done');
      const after = await active.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
      expect(after.phases).toHaveLength(2);
      expect(after.phases[1]?.threadId).toBe(before.phases[0]?.threadId);
    } finally {
      await active?.close();
      await rm(dataDir, { recursive: true, force: true });
    }
  });
}
