// A worktree removed OUTSIDE the app while its thread's turn runs
// (`git worktree remove` in a terminal), seen by the registry watch
// (internal/app/app_worktree_watch.go).
//
// The mock holds the turn open at a gate, and a message queued behind it has
// been flushed to the provider but not yet taken up (the CLI's turn-boundary
// pickup). After the outside removal: the session is stopped and not
// restarted, the running turn ends interrupted, the row moves to Base, the
// thread gets a warning notice saying the worktree was removed outside the
// app, and the queued message is back in the composer instead of lost.
// Sending it starts a new session whose working directory is the project
// root, and it is sent once.
import { test, expect, type HarnessMockEvent } from './fixtures.js';
import {
  RESULT_LINE,
  claudeTurnsScenario,
  emit,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';
import {
  attachWorktree,
  basename,
  errorToasts,
  harnessGit,
  openThread,
  samePath,
  seedWorktreeProject,
  threadRow,
} from './worktree-removal-helpers.js';

interface TurnCompleted {
  threadId: string;
  stopReason: string;
  aborted?: boolean;
}

const TITLE = 'Busy worktree thread';
const QUEUED = 'Queued behind the long job: also update the changelog.';

test('removing a busy thread\'s worktree outside the app interrupts the turn, moves the thread to Base and keeps its queued message', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);
  const project = await seedWorktreeProject(harness, 'worktree-external-removal', [TITLE], ['busy-wt']);
  const [threadId] = project.threadIds;
  const bound = await attachWorktree(harness, threadId, 'busy-wt');
  const worktreePath = bound.worktreePath!;

  // Turn 1 holds; a mid-turn message waits for the turn boundary.
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('external-removal-hold', [
      [emit(textLines('msg-working', 'Working on the long job.')), { waitSignal: { name: 'hold' } }, emit([RESULT_LINE])],
    ], { queuedInputAtBoundary: true }),
  });

  await harness.open(page);
  await openThread(page, TITLE);
  await expect(page.getByTestId('env-picker-trigger')).toHaveText(basename(worktreePath));

  const input = page.getByLabel('Message Input');
  await input.fill('Start the long job.');
  await input.press('Enter');
  const first = await harness.waitForEvent<HarnessMockEvent>('harness:mock', (ev) => ev.report.kind === 'registered');
  expect(await samePath(first.cwd, worktreePath), 'the first session did not start in the worktree').toBe(true);
  await waitForGate(harness, 'hold');
  await expect(page.getByText('Working on the long job.')).toBeVisible();

  const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
  await input.fill(QUEUED);
  await input.press('Enter');
  await flushed;
  const queuedPreview = page.getByTestId('send-queue-preview-row').filter({ hasText: QUEUED });
  await expect(queuedPreview).toBeVisible();

  // Outside the app: git removes the checkout from the project root.
  const interrupted = harness.waitForEvent<TurnCompleted>('provider:turn_completed', (ev) => ev.threadId === threadId);
  harnessGit(harness, project.root, 'worktree', 'remove', '--force', worktreePath);

  // The running turn ends interrupted.
  const completed = await interrupted;
  expect(completed.aborted || completed.stopReason === 'interrupted', `turn ended ${JSON.stringify(completed)}`).toBe(true);

  // The row moves to Base, on the backend and in the composer.
  await expect.poll(async () => samePath((await threadRow(harness, threadId)).workspacePath, project.root)).toBe(true);
  expect((await threadRow(harness, threadId)).worktreePath ?? '').toBe('');
  await expect(page.getByTestId('env-picker-trigger')).toHaveText('Base');

  // The thread says what happened, at its own position in the timeline.
  await expect(page.getByText(/Worktree .*busy-wt was removed outside Agent Overflow\./)).toBeVisible();
  await expect(page.getByText(/this thread now runs in Base/)).toBeVisible();

  // The queued message is back in the composer, not lost and not sent.
  await expect(queuedPreview).toHaveCount(0);
  await expect(input).toHaveValue(QUEUED);
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: QUEUED })).toHaveCount(0);

  // Nothing restarted the session.
  expect(harness.countEvents<HarnessMockEvent>('harness:mock', (ev) => ev.report.kind === 'registered')).toBe(1);

  // Sending it starts a new session in the project root, and it is sent once.
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('external-removal-after', [
      [emit([...textLines('msg-after', 'Running in Base now.'), RESULT_LINE])],
    ]),
  });
  await input.press('Enter');
  const second = await harness.waitForEvent<HarnessMockEvent>('harness:mock', (ev) => ev.report.kind === 'registered');
  expect(await samePath(second.cwd, project.root), 'the next session did not start in the project root').toBe(true);
  await expect(page.getByText('Running in Base now.')).toBeVisible();
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: QUEUED })).toHaveCount(1);
  await expect(errorToasts(page)).toHaveCount(0);
});
