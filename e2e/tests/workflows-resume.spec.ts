import { test, expect } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
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
      { label: 'question', steps: [{ emit: { lines: [questionResult('Which option?')] } }] },
      { label: 'done', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
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
      { label: 'done', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
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
    expect(after.phases).toHaveLength(2);
    expect(after.phases[1]?.threadId).not.toBe(parkedThread);
  });
}
