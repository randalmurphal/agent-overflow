// Since 2.1.299 Claude Code leaves auto mode when a session switches to a
// model without it, and reports the mode it now runs on system/status. The
// mock provider models that (cmd/ao-mockprovider/claude_permission_mode.go).
//
// Coverage: a Claude thread on Auto switches to claude-sonnet-4-6, whose
// remembered settings carry Auto, while its session runs; the access picker then shows Supervised, the thread row
// says approval-required, a warning notice names the model and the change,
// and the next turn runs on the same session.
import { test, expect } from './fixtures.js';
import { RESULT_LINE, emit, textLines } from './agent-visibility-helpers.js';
import { openThread } from './worktree-removal-helpers.js';
import { mockSessions } from './workflows-helpers.js';
import type { SeedResult } from './fixtures.js';

const TITLE = 'Auto thread switching model';
const NOTICE = "Claude Code turned off Auto on claude-sonnet-4-6. This thread's access is now Supervised.";

test('switching an auto session to a model without auto mode shows the mode Claude runs', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'auto-mode-model-switch',
      provider: 'claude',
      turns: [
        { label: 'first', steps: [emit([...textLines('msg-first', 'First answer.'), RESULT_LINE])] },
        { label: 'second', steps: [emit([...textLines('msg-second', 'Second answer.'), RESULT_LINE])] },
      ],
      afterTurns: 'silent',
    },
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'auto-mode-switch',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [
          {
            title: TITLE,
            provider: 'claude',
            runtimeMode: 'auto',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
          {
            title: 'Earlier Sonnet 4.6 thread',
            provider: 'claude',
            model: 'claude-sonnet-4-6',
            turns: [{ userText: 'older work', items: [{ kind: 'assistant_text', summary: 'Done.' }] }],
          },
        ],
      },
    ],
  });
  const [threadId, sonnetThreadId] = seed.projects[0].threadIds;
  // A model switch applies the model's remembered settings. Auto picked on a
  // Sonnet 4.6 thread before 2.1.299 is what carries Auto into this switch.
  await harness.rpc('UpdateThreadRuntimeMode', sonnetThreadId, 'auto');
  // Same effort on both sides, so the switch needs no /effort, which the
  // mock CLI does not advertise.
  await harness.rpc('UpdateThreadReasoningEffort', threadId, 'high');

  await harness.open(page);
  await openThread(page, TITLE);
  const access = page.getByTestId('composer-access-toggle');
  await expect(access).toHaveAttribute('data-mode', 'auto');

  await harness.rpc('StartSession', threadId);
  const [mock] = await mockSessions(harness, 'claude', 1);
  expect(mock.sessionConfig?.permissionMode).toBe('auto');
  await harness.rpc('SendMessage', threadId, 'first', null);
  await expect(page.getByText('First answer.', { exact: true })).toBeVisible();

  await harness.rpc('UpdateThreadModel', threadId, 'claude-sonnet-4-6');
  // The switch brings the model's remembered Auto (set up above), so the
  // reconcile is a bare set_model on the running auto session.
  expect(mock.mockId).toBe((await mockSessions(harness, 'claude', 1))[0].mockId);

  await expect(access).toHaveAttribute('data-mode', 'approval-required');
  await expect(access).toHaveAttribute('aria-label', 'Runtime Access Mode: Supervised');
  await expect(page.getByTestId('notification-row').filter({ hasText: NOTICE })).toHaveCount(1);
  await expect
    .poll(async () => (await harness.rpc<{ runtimeMode: string }>('GetThread', threadId)).runtimeMode)
    .toBe('approval-required');

  // The next turn runs on the same process: AO asks the CLI for nothing it
  // would refuse, and no restart was needed.
  await harness.rpc('SendMessage', threadId, 'second', null);
  await expect(page.getByText('Second answer.', { exact: true })).toBeVisible();
  expect(await mockSessions(harness, 'claude', 1)).toHaveLength(1);
  await expect(page.getByText('auto mode unavailable', { exact: false })).toHaveCount(0);

  await page.getByTestId('notification-row').filter({ hasText: NOTICE }).scrollIntoViewIfNeeded();
  for (const scheme of ['dark', 'light'] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await page.screenshot({ path: test.info().outputPath(`claude-auto-mode-model-switch-${scheme}.png`) });
  }
});
