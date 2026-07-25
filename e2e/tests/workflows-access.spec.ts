import { test, expect } from './fixtures.js';
import {
  doneResult,
  seedWorkflowProject,
  sessionConfigs,
  setClaudeScenario,
  start,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

// A phase's `access` declaration is enforced at the provider session, not just
// used to decide whether a worktree is cut (spec §9, decision D22). These
// specs assert the configuration the app actually launched each provider
// process with, observed by the mock itself — the Go unit tests cover the
// mapping, and this covers the wiring surviving all the way to argv /
// thread-start params.
//
// Each fixture runs ONE workflow whose read-only phase is followed by a write
// phase. Because a writing phase exists, the whole run gets a worktree and
// both phases execute in it, so the two sessions cannot be told apart by
// workspace: any difference in their launch config came from `access`.
// Phases run sequentially, so mock registration order is phase order.

const phase = (id: string, provider: string, access: string, next: string) =>
  `  - id: ${id}
    driver: agent
    provider: ${provider}
    model: ${provider === 'codex' ? 'gpt-5.5' : 'claude-opus-4-7'}
    prompt: ${id}.md
    access: ${access}
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: ${next}
`;

const mixedAccessYaml = (name: string, provider: string) => `id: ${name}
name: Mixed access
inputs:
  goal:
    schema:
      type: string
phases:
${phase('survey', provider, 'read-only', 'apply')}${phase('apply', provider, 'write', 'done')}cleanup: manual
`;

test('claude phases launch with the permission config their access declares', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'access-claude', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'workflow-access-claude-project',
    [
      {
        name: 'access-claude',
        yaml: mixedAccessYaml('access-claude', 'claude'),
        prompts: {
          'survey.md': 'Survey the repository and return the envelope.',
          'apply.md': 'Apply the change and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'access-claude');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((p) => p.phaseId)).toEqual(['survey', 'apply']);

  const [survey, apply] = await sessionConfigs(harness, 'claude', 2);

  // Read-only: denies rather than prompts, AND the write tools are removed
  // outright so no settings-sourced allow rule can reinstate them.
  expect(survey.permissionMode).toBe('dontAsk');
  expect(survey.disallowedTools).toEqual(['Write', 'Edit', 'NotebookEdit']);

  // Write: full access inside the run's isolated worktree.
  expect(apply.permissionMode).toBe('bypassPermissions');
  expect(apply.disallowedTools ?? []).toEqual([]);
});

test('codex phases launch with the sandbox their access declares', async ({ harness }) => {
  const envelope = JSON.stringify({
    status: 'done',
    outputs: { complete: true },
    question: null,
    reason: null,
  });
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'access-codex',
      provider: 'codex',
      turns: [
        {
          steps: [
            {
              emit: {
                lines: [
                  '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}',
                  `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"agentMessage","id":"msg-\${TURN}","status":"completed","text":${JSON.stringify(envelope)}}}}`,
                  '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}',
                ],
              },
            },
          ],
        },
      ],
      afterTurns: 'repeatLast',
    },
  });
  const project = await seedWorkflowProject(
    harness,
    'workflow-access-codex-project',
    [
      {
        name: 'access-codex',
        yaml: mixedAccessYaml('access-codex', 'codex'),
        prompts: {
          'survey.md': 'Survey the repository and return the envelope.',
          'apply.md': 'Apply the change and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'access-codex');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((p) => p.phaseId)).toEqual(['survey', 'apply']);

  const [survey, apply] = await sessionConfigs(harness, 'codex', 2);

  // Read-only: the OS sandbox refuses writes, and `never` means a sandbox
  // denial returns to the model instead of escalating to an absent human.
  expect(survey.sandbox).toBe('read-only');
  expect(survey.approvalPolicy).toBe('never');

  // Write: full access inside the run's isolated worktree.
  expect(apply.sandbox).toBe('danger-full-access');
  expect(apply.approvalPolicy).toBe('never');
});
