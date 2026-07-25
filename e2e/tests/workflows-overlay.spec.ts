// The workflows overlay, driven as a human drives it (UI-SPEC
// docs/specs/workflows-system-ui/UI-SPEC.md). Every case here clicks and types
// in the REAL overlay — sidebar footer, home, run detail, sweep, loss preview,
// intake, the §8 keys — against the real engine with mocked providers. Backend
// setup goes through RPCs; assertions are DOM state or wire events, never a
// stubbed store.
//
// The overlay is a sibling of the pane host, so nothing here navigates panes:
// opening a thread is the one action that breaks out of the overlay (R3), and
// it is covered by the `/workflow` case, which never opens the overlay at all.
import { test, expect, type SeedResult } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  questionResult,
  seedWorkflow,
  setClaudeScenario,
  singlePhaseWorkflow,
  startWorkflow,
  waitForWorkflowState,
} from './workflows-helpers.js';

/** The chord `mod+shift+k` resolves to on the browser Playwright drives. */
const MOD = process.platform === 'darwin' ? 'Meta' : 'Control';

const oneDoneTurn = [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }];

test('the sidebar footer opens home and a parked gate resolves from its detail', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-gate', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-gate-project',
    'gate-flow',
    humanGateWorkflow('gate-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'gate-flow', 'Port the parser');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  await page.goto(harness.url);
  // §6: one footer row, one count, amber only because a human is blocked.
  await expect(page.getByTestId('sidebar-workflows-attention')).toHaveText('1');

  await page.getByTestId('sidebar-workflows-button').click();
  await expect(page.getByTestId('workflows-overlay')).toBeVisible();

  // §3.2: project group → needs-attention list → this run, carrying its state
  // word and nothing else about the machinery behind it (R2).
  await expect(page.getByTestId('workflow-project-group')).toContainText('overlay-gate-project');
  const row = page.getByTestId('workflow-run-row').filter({ hasText: 'Port the parser' });
  await expect(row).toContainText('Review gate');
  await row.click();

  // §4.1 header + §4.2 tree + §4.3 action row.
  const detail = page.getByTestId('workflow-run-detail');
  await expect(detail).toHaveAttribute('data-item-id', item.id);
  await expect(page.getByTestId('workflow-run-state')).toHaveText('Review gate');
  await expect(page.getByTestId('workflow-run-title')).toHaveText('Port the parser');
  await expect(page.getByTestId('workflow-digest')).toContainText('What happened');
  await expect(page.getByTestId('workflow-phase-row').first()).toContainText('plan');

  // The primary names the phase the gate routes to — read off the definition,
  // not invented.
  const approve = page.getByTestId('workflow-action').filter({ hasText: 'Approve → apply' });
  const done = waitForWorkflowState(harness, item.id, 'done');
  await approve.click();
  await done;

  // §4.4: the last parked run resolved, so the sweep is exhausted.
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('1 approved');
});

test('the sweep steps with j / k, auto-advances past a receipt, and lands on all clear', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-sweep', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-sweep-project',
    'sweep-flow',
    humanGateWorkflow('sweep-flow'),
  );
  // Sequential so the sweep order (oldest parked first) is a fact, not a race.
  const first = await startWorkflow(harness, project.projectId, 'sweep-flow', 'First parked run');
  await waitForWorkflowState(harness, first.id, 'needs-human', 'gate');
  const second = await startWorkflow(harness, project.projectId, 'sweep-flow', 'Second parked run');
  await waitForWorkflowState(harness, second.id, 'needs-human', 'gate');

  await page.goto(harness.url);
  await expect(page.getByTestId('sidebar-workflows-attention')).toHaveText('2');
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'First parked run' }).click();
  const detail = page.getByTestId('workflow-run-detail');
  const counter = page.getByTestId('workflow-sweep-counter');
  await expect(detail).toHaveAttribute('data-item-id', first.id);
  await expect(counter).toHaveText('1 of 2');

  // §8: j / k step the sweep without touching the mouse, and wrap.
  await page.keyboard.press('j');
  await expect(detail).toHaveAttribute('data-item-id', second.id);
  await expect(counter).toHaveText('2 of 2');
  await page.keyboard.press('k');
  await expect(detail).toHaveAttribute('data-item-id', first.id);

  const firstDone = waitForWorkflowState(harness, first.id, 'done');
  await page.getByTestId('workflow-action').filter({ hasText: 'Approve' }).first().click();
  await firstDone;
  // The receipt holds the resolved run on screen long enough to read; the sweep
  // then steps itself to the run that still needs a human (§4.4).
  await expect(detail).toHaveAttribute('data-item-id', second.id);

  const secondDone = waitForWorkflowState(harness, second.id, 'done');
  await page.getByTestId('workflow-action').filter({ hasText: 'Approve' }).first().click();
  await secondDone;
  await expect(page.getByTestId('workflow-all-clear')).toBeVisible();
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('2 approved');
});

test('discard previews exactly what it would destroy before it destroys it', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-discard', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-discard-project',
    'discard-flow',
    singlePhaseWorkflow('discard-flow', '        - to: done', 'write'),
  );
  const item = await startWorkflow(harness, project.projectId, 'discard-flow', 'Finished run');
  await waitForWorkflowState(harness, item.id, 'done');

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Finished run' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toHaveAttribute('data-item-id', item.id);

  // §4.5 / D23: the row's Discard opens the loss preview. It does not destroy.
  await page.getByTestId('workflow-action').filter({ hasText: 'Discard' }).click();
  const dialog = page.getByTestId('workflow-discard-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('workflow-discard-worktree')).toHaveCount(1);
  await expect(dialog).toContainText('The run record is kept.');
  const stillThere = await harness.rpc<{ item: { state: string } }>('WorkflowGetItem', item.id);
  expect(stillThere.item.state).toBe('done');

  await page.getByTestId('workflow-discard-confirm').click();
  await expect(page.getByRole('alert')).toContainText('Discarded');
  await expect(page.getByTestId('workflow-all-clear-summary')).toContainText('1 discarded');
});

test('New run starts a workflow from the overlay', async ({ harness, page }) => {
  await setClaudeScenario(harness, 'overlay-intake', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-intake-project',
    'intake-flow',
    singlePhaseWorkflow('intake-flow', '        - to: done'),
  );

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflows-new-run').click();

  // §5.1: Project · Goal · Workflow · Base branch · step mode, primary `Start`.
  await expect(page.getByTestId('workflow-intake-dialog')).toBeVisible();
  await expect(page.getByTestId('workflow-intake-submit')).toBeDisabled();
  await page.getByTestId('workflow-intake-goal').fill('Start from the overlay');
  await page
    .locator('[data-testid="workflow-intake-workflow"][data-workflow-id="intake-flow"]')
    .click();
  // The workflow's own declared field is a plain form field (R2), and Start
  // stays refused until it has a value — with the field named, not the schema.
  await expect(page.getByTestId('workflow-intake-error')).toContainText('goal');
  await page.getByTestId('workflow-seed-goal').fill('Start from the overlay');

  const started = harness.waitForEvent<{ projectId: string }>(
    'workflow:item-state',
    (event) => event.projectId === project.projectId,
  );
  await page.getByTestId('workflow-intake-submit').click();
  await started;
  await expect(page.getByRole('alert')).toContainText('Started — intake-flow');
  await expect(
    page.getByTestId('workflow-run-row').filter({ hasText: 'Start from the overlay' }),
  ).toBeVisible();
});

test('a question answers from the footer input, and typing there never fires the §8 keys', async ({
  harness,
  page,
}) => {
  await setClaudeScenario(harness, 'overlay-question', [
    { label: 'question', steps: [{ emit: { lines: [questionResult('Which option?')] } }] },
    { label: 'answer', steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflow(
    harness,
    'overlay-question-project',
    'ask-flow',
    singlePhaseWorkflow('ask-flow', '        - to: done'),
  );
  const item = await startWorkflow(harness, project.projectId, 'ask-flow', 'Needs an answer');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'question');

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Needs an answer' }).click();
  await expect(page.getByTestId('workflow-question')).toContainText('Which option?');

  // §8: `a` on a question focuses the answer box rather than committing —
  // there is nothing to commit until it has text.
  const answer = page.getByTestId('workflow-answer-input');
  await page.keyboard.press('a');
  await expect(answer).toBeFocused();

  // What follows is text, not commands. Without the editable-target guard the
  // `t` in "option" would take the run over and tear this surface down.
  await page.keyboard.type('use option A');
  await expect(answer).toHaveValue('use option A');
  await expect(page.getByTestId('workflow-run-detail')).toBeVisible();

  const done = waitForWorkflowState(harness, item.id, 'done');
  await page.keyboard.press('Enter');
  await done;
});

test('/workflow inserts the workflow context block below an in-progress draft', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'overlay-composer-project',
        repo: {},
        workflows: {
          definitions: [
            {
              name: 'ctx-flow',
              yaml: singlePhaseWorkflow('ctx-flow', '        - to: done'),
              prompts: { 'ctx-flow.md': 'Complete this phase.' },
            },
          ],
        },
        threads: [
          { title: 'Composer thread', turns: [{ userText: 'hello there', items: [] }] },
        ],
      },
    ],
  });
  expect(seed.projects[0].threadIds).toHaveLength(1);

  await page.goto(harness.url);
  await page.getByText('Composer thread').click();
  const composer = page.getByLabel('Message Input');
  await composer.fill('draft in progress');

  await page.keyboard.press(`${MOD}+Shift+KeyK`);
  const query = page.getByTestId('command-palette-input');
  await query.click();
  await query.fill('Insert /workflow Context');
  await page.getByRole('option').filter({ hasText: 'Insert /workflow Context' }).click();

  // D17: nothing workflow-shaped is in context until it is invoked, and the
  // block lands BELOW whatever was already drafted.
  await expect(composer).toHaveValue(/^draft in progress\n\nAgent Overflow workflows/);
  await expect(composer).toHaveValue(/Configured workflows:[\s\S]*ctx-flow/);
});

test('a view-only session disables every mutating affordance', async ({ harness, page }) => {
  await setClaudeScenario(harness, 'overlay-remote', oneDoneTurn);
  const project = await seedWorkflow(
    harness,
    'overlay-remote-project',
    'remote-flow',
    humanGateWorkflow('remote-flow'),
  );
  const item = await startWorkflow(harness, project.projectId, 'remote-flow', 'Remote parked run');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  // The manifest's `remote` bit is computed from the peer's locality and the
  // harness binds loopback only, so a LAN peer cannot be produced from here.
  // Patching that one field on the wire hands the SPA exactly the manifest a
  // remote browser would receive; everything downstream of it is production
  // code, including the wsClient validation that publishes the bit.
  await page.route('**/bootstrap.json*', async (route) => {
    const response = await route.fetch();
    const manifest = (await response.json()) as Record<string, unknown>;
    await route.fulfill({ response, body: JSON.stringify({ ...manifest, remote: true }) });
  });

  await page.goto(harness.url);
  await page.getByTestId('sidebar-workflows-button').click();

  // §10: home's controls all mutate, so all of them go dead with one reason.
  await expect(page.getByTestId('workflows-pause-all')).toBeDisabled();
  const newRun = page.getByTestId('workflows-new-run');
  await expect(newRun).toBeDisabled();
  await expect(newRun).toHaveAttribute('title', 'Local only');
  // The project filter is view state, not a mutation — it stays live.
  await expect(page.getByTestId('workflows-project-filter')).toBeEnabled();

  await page.getByTestId('workflow-run-row').filter({ hasText: 'Remote parked run' }).click();
  const actions = page.getByTestId('workflow-action');
  await expect(actions.first()).toBeVisible();
  for (const action of await actions.all()) {
    await expect(action).toBeDisabled();
    await expect(action).toHaveAttribute('title', 'Local only');
  }

  // The guard is not just visual: the §8 keys are refused too, so the run is
  // still parked afterwards.
  await page.keyboard.press('a');
  const after = await harness.rpc<{ item: { state: string } }>('WorkflowGetItem', item.id);
  expect(after.item.state).toBe('needs-human');
});
