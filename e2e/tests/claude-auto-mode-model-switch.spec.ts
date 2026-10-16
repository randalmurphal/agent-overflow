// Since 2.1.299 Claude Code leaves auto mode when a session switches to a
// model without it, and reports the mode it now runs on system/status. In
// plan it reports nothing and refuses auto when plan ends. The mock provider
// models both (cmd/ao-mockprovider/claude_permission_mode.go).
//
// Coverage:
//   1. A Claude thread on Auto switches to claude-sonnet-4-6, whose
//      remembered settings carry Auto, while its session runs: the access
//      picker shows Supervised, the thread row says approval-required, a
//      warning notice names the model and the change, and the next turn
//      runs on the same session. Switching back to the first model restores
//      its remembered Auto with a notice of its own, and the picker shows
//      Auto again.
//   2. The same switch made in plan, after a plan turn: the session restarts
//      on the new model, and the first turn after plan lands on Supervised
//      with the same notice and is answered on that session. Switching back
//      in plan restores Auto with the restore notice once.
//   3. A switch back to the first model made in plan, on a session started
//      in chat, is applied by a restart; the restore notice still shows
//      once, at the switch, and the next turn runs on Auto.
//   4. A Supervised thread switched to claude-sonnet-4-6, whose remembered
//      Auto the CLI refuses: the drop notice shows, the restore notice does
//      not, and the thread lands on Supervised on the same process.
import { test, expect } from './fixtures.js';
import { RESULT_LINE, advance, emit, textLines, waitForGate } from './agent-visibility-helpers.js';
import { openThread } from './worktree-removal-helpers.js';
import { mockSessions } from './workflows-helpers.js';
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

const SONNET_46 = 'claude-sonnet-4-6';
const DROP_NOTICE = `Claude Code turned off Auto on ${SONNET_46}. This thread's access is now Supervised.`;

/** One scenario turn per entry: a text answers the turn; a [text, gate]
 * pair writes the text, then waits for the gate before the result. */
function answers(...turns: Array<string | [string, string]>) {
  return turns.map((turn, i) => {
    const [text, gate] = typeof turn === 'string' ? [turn, undefined] : turn;
    const lines = textLines(`msg-${i + 1}`, text);
    return {
      label: `turn-${i + 1}`,
      steps: gate ? [emit(lines), { waitSignal: { name: gate } }, emit([RESULT_LINE])] : [emit([...lines, RESULT_LINE])],
    };
  });
}

/** An Auto thread whose current model and claude-sonnet-4-6 both remember
 * Auto, opened on its page. */
async function seedAutoThread(
  harness: HarnessApp,
  page: Page,
  title: string,
  turns: Array<string | [string, string]>,
): Promise<string> {
  await harness.rpc('HarnessSetScenario', {
    scenario: { version: 1, name: `auto-mode-${title}`, provider: 'claude', turns: answers(...turns), afterTurns: 'silent' },
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'auto-mode-switch',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [
          {
            title,
            provider: 'claude',
            runtimeMode: 'auto',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
          {
            title: 'Earlier Sonnet 4.6 thread',
            provider: 'claude',
            model: SONNET_46,
            turns: [{ userText: 'older work', items: [{ kind: 'assistant_text', summary: 'Done.' }] }],
          },
        ],
      },
    ],
  });
  const [threadId, sonnetThreadId] = seed.projects[0].threadIds;
  // A model switch applies the model's remembered settings. Auto picked on
  // each model is what the switches below carry.
  await harness.rpc('UpdateThreadRuntimeMode', threadId, 'auto');
  await harness.rpc('UpdateThreadRuntimeMode', sonnetThreadId, 'auto');
  // Same effort on both sides, so a switch needs no /effort, which the mock
  // CLI does not advertise.
  await harness.rpc('UpdateThreadReasoningEffort', threadId, 'high');

  await harness.open(page);
  await openThread(page, title);
  await expect(page.getByTestId('composer-access-toggle')).toHaveAttribute('data-mode', 'auto');
  return threadId;
}

async function runtimeMode(harness: HarnessApp, threadId: string): Promise<string> {
  return (await harness.rpc<{ runtimeMode: string }>('GetThread', threadId)).runtimeMode;
}

async function screenshots(page: Page, name: string) {
  for (const scheme of ['dark', 'light'] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    // The scheme change fades colors in; capture after it settles.
    await page.waitForFunction(() => document.getAnimations().every((a) => a.playState !== 'running'));
    await page.screenshot({ path: test.info().outputPath(`${name}-${scheme}.png`) });
  }
}

test('switching an auto session to a model without auto mode shows the mode Claude runs, and back restores Auto', async ({
  harness,
  page,
}) => {
  const threadId = await seedAutoThread(harness, page, 'Auto thread switching model', [
    'First answer.',
    'Second answer.',
    'Third answer.',
  ]);
  const firstModel = (await harness.rpc<{ model: string }>('GetThread', threadId)).model;
  const access = page.getByTestId('composer-access-toggle');
  const notices = page.getByTestId('notification-row');

  await harness.rpc('StartSession', threadId);
  const [mock] = await mockSessions(harness, 'claude', 1);
  expect(mock.sessionConfig?.permissionMode).toBe('auto');
  await harness.rpc('SendMessage', threadId, 'first', null);
  await expect(page.getByText('First answer.', { exact: true })).toBeVisible();

  await harness.rpc('UpdateThreadModel', threadId, SONNET_46);
  // The switch brings the model's remembered Auto, so the reconcile is a
  // bare set_model on the running auto session.
  expect(mock.mockId).toBe((await mockSessions(harness, 'claude', 1))[0].mockId);

  await expect(access).toHaveAttribute('data-mode', 'approval-required');
  await expect(access).toHaveAttribute('aria-label', 'Runtime Access Mode: Supervised');
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect.poll(() => runtimeMode(harness, threadId)).toBe('approval-required');

  // The next turn runs on the same process: AO asks the CLI for nothing it
  // would refuse, and no restart was needed.
  await harness.rpc('SendMessage', threadId, 'second', null);
  await expect(page.getByText('Second answer.', { exact: true })).toBeVisible();
  expect(await mockSessions(harness, 'claude', 1)).toHaveLength(1);
  await expect(page.getByText('auto mode unavailable', { exact: false })).toHaveCount(0);

  // Back to the first model: its remembered Auto returns, and says so.
  const restoreNotice = `Auto is back on for ${firstModel}. This thread's access is Auto again.`;
  await harness.rpc('UpdateThreadModel', threadId, firstModel);
  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'auto');
  await expect(access).toHaveAttribute('aria-label', 'Runtime Access Mode: Auto');
  expect(await runtimeMode(harness, threadId)).toBe('auto');
  // Each notice sits where its switch happened.
  await expect(notices).toContainText([DROP_NOTICE, restoreNotice]);

  await harness.rpc('SendMessage', threadId, 'third', null);
  await expect(page.getByText('Third answer.', { exact: true })).toBeVisible();
  expect(await mockSessions(harness, 'claude', 1)).toHaveLength(1);

  await notices.last().scrollIntoViewIfNeeded();
  await screenshots(page, 'claude-auto-mode-model-switch-v2');
});

test('switching models in plan drops Auto when plan ends and restores it, with one notice each', async ({
  harness,
  page,
}) => {
  const threadId = await seedAutoThread(harness, page, 'Auto thread planning', ['Here is the plan.']);
  const firstModel = (await harness.rpc<{ model: string }>('GetThread', threadId)).model;
  const restoreNotice = `Auto is back on for ${firstModel}. This thread's access is Auto again.`;
  const access = page.getByTestId('composer-access-toggle');
  const notices = page.getByTestId('notification-row');

  await harness.rpc('StartSession', threadId);
  const [first] = await mockSessions(harness, 'claude', 1);
  await harness.rpc('UpdateThreadMode', threadId, 'plan');
  await harness.rpc('SendMessage', threadId, 'plan it', null);
  await expect(page.getByText('Here is the plan.', { exact: true })).toBeVisible();
  // The restarted process below plays its own first turn.
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'auto-mode-after-plan',
      provider: 'claude',
      turns: answers('Done after plan.', 'Back on Auto.'),
      afterTurns: 'silent',
    },
  });

  // A model switch in plan is not applied live: the session restarts on
  // the new model once the thread is quiet, started on auto, which the CLI
  // cannot run on this model. Nothing reports that until the next turn.
  await harness.rpc('UpdateThreadModel', threadId, SONNET_46);
  let restarted: { mockId: string; sessionConfig?: { permissionMode?: string } } | undefined;
  await expect
    .poll(async () => {
      restarted = (await mockSessions(harness, 'claude', 1)).find((mock) => mock.mockId !== first.mockId);
      return restarted?.sessionConfig?.permissionMode;
    })
    .toBe('auto');
  await harness.rpc('UpdateThreadMode', threadId, 'chat');

  // The first turn after plan: the CLI's init says it runs default, and
  // the thread follows before the answer.
  await harness.rpc('SendMessage', threadId, 'go ahead', null);
  await expect(page.getByText('Done after plan.', { exact: true })).toBeVisible();
  await expect(page.getByTestId('notification-row').filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'approval-required');
  await expect.poll(() => runtimeMode(harness, threadId)).toBe('approval-required');
  const sessions = await mockSessions(harness, 'claude', 1);
  // No third process: the turn after plan ran on the restarted one.
  expect(sessions.map((mock) => mock.mockId).filter((id) => id !== first.mockId)).toEqual([restarted!.mockId]);
  await expect(page.getByText('auto mode unavailable', { exact: false })).toHaveCount(0);

  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(0);

  // Back to the first model, again in plan. This process started in plan,
  // so the switch applies live; its remembered Auto comes back with its
  // notice, and plan returns to it.
  await harness.rpc('UpdateThreadMode', threadId, 'plan');
  await harness.rpc('UpdateThreadModel', threadId, firstModel);
  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'auto');
  await harness.rpc('UpdateThreadMode', threadId, 'chat');
  await harness.rpc('SendMessage', threadId, 'carry on', null);
  await expect(page.getByText('Back on Auto.', { exact: true })).toBeVisible();
  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(1);
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'auto');
  expect(await runtimeMode(harness, threadId)).toBe('auto');
  await expect(notices).toContainText([DROP_NOTICE, restoreNotice]);

  await notices.last().scrollIntoViewIfNeeded();
  await screenshots(page, 'claude-auto-mode-plan-switch-v3');
});

test('switching back to an auto model in plan restores Auto through the restart, with one notice', async ({
  harness,
  page,
}) => {
  const threadId = await seedAutoThread(harness, page, 'Auto thread back in plan', ['First answer.']);
  const firstModel = (await harness.rpc<{ model: string }>('GetThread', threadId)).model;
  const restoreNotice = `Auto is back on for ${firstModel}. This thread's access is Auto again.`;
  const access = page.getByTestId('composer-access-toggle');
  const notices = page.getByTestId('notification-row');

  await harness.rpc('StartSession', threadId);
  const [first] = await mockSessions(harness, 'claude', 1);
  await harness.rpc('SendMessage', threadId, 'first', null);
  await expect(page.getByText('First answer.', { exact: true })).toBeVisible();
  await harness.rpc('UpdateThreadModel', threadId, SONNET_46);
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'approval-required');

  // This process started in chat, so a switch in plan is applied by a
  // restart once the thread is quiet. The thread's access is Auto from the
  // switch on, and the notice says so then.
  await harness.rpc('HarnessSetScenario', {
    scenario: { version: 1, name: 'auto-mode-back-restart', provider: 'claude', turns: answers('Back on Auto.'), afterTurns: 'silent' },
  });
  await harness.rpc('UpdateThreadMode', threadId, 'plan');
  await harness.rpc('UpdateThreadModel', threadId, firstModel);
  await expect(access).toHaveAttribute('data-mode', 'auto');
  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(1);
  let restarted: { mockId: string; sessionConfig?: { permissionMode?: string } } | undefined;
  await expect
    .poll(async () => {
      restarted = (await mockSessions(harness, 'claude', 1)).find((mock) => mock.mockId !== first.mockId);
      return restarted?.sessionConfig?.permissionMode;
    })
    .toBe('auto');

  await harness.rpc('UpdateThreadMode', threadId, 'chat');
  await harness.rpc('SendMessage', threadId, 'carry on', null);
  await expect(page.getByText('Back on Auto.', { exact: true })).toBeVisible();
  await expect(notices.filter({ hasText: restoreNotice })).toHaveCount(1);
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'auto');
  expect(await runtimeMode(harness, threadId)).toBe('auto');

  await notices.last().scrollIntoViewIfNeeded();
  await screenshots(page, 'claude-auto-mode-plan-restart-v3');
});

test('switching a supervised session to a model without auto mode that remembers Auto lands on Supervised, with no restore notice', async ({
  harness,
  page,
}) => {
  const threadId = await seedAutoThread(harness, page, 'Supervised thread switching model', ['First answer.', 'Second answer.']);
  const access = page.getByTestId('composer-access-toggle');
  const notices = page.getByTestId('notification-row');
  await harness.rpc('UpdateThreadRuntimeMode', threadId, 'approval-required');
  await expect(access).toHaveAttribute('data-mode', 'approval-required');

  await harness.rpc('StartSession', threadId);
  const [mock] = await mockSessions(harness, 'claude', 1);
  await harness.rpc('SendMessage', threadId, 'first', null);
  await expect(page.getByText('First answer.', { exact: true })).toBeVisible();

  // The switch brings Sonnet 4.6's remembered Auto, which the CLI refuses
  // for that model: the thread takes the drop and its notice, not the
  // restore notice, on the same process.
  await harness.rpc('UpdateThreadModel', threadId, SONNET_46);
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
  await expect(access).toHaveAttribute('data-mode', 'approval-required');
  await expect.poll(() => runtimeMode(harness, threadId)).toBe('approval-required');
  await harness.rpc('SendMessage', threadId, 'second', null);
  await expect(page.getByText('Second answer.', { exact: true })).toBeVisible();
  expect((await mockSessions(harness, 'claude', 1)).map((m) => m.mockId)).toEqual([mock.mockId]);
  await expect(notices.filter({ hasText: 'Auto is back on' })).toHaveCount(0);
  await expect(notices.filter({ hasText: DROP_NOTICE })).toHaveCount(1);
});
