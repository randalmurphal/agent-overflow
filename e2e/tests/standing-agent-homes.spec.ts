// A standing agent's home (docs/specs/standing-agents.md, "Standing agent"),
// end to end on both mock providers.
//
// Coverage: promotion creates `<config>/agents/<thread-id>/` with a README
// naming the agent, its thread id and project; the home block (home path,
// thread id, the sentence that the agent owns the directory) reaches the
// provider on a start and on a resume, as Claude's appended system prompt
// and as Codex's developer instructions, with the app guide switched off so
// the block cannot ride on it; a demoted thread's next session carries no
// block and keeps its home on disk; the Demote confirm's "Demote and
// Delete Home" removes the home and leaves an ordinary row; and one whose
// home cannot be deleted still demotes, with a toast saying the folder was
// not deleted and the folder named in its details.
import { chmodSync, existsSync, mkdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { capture } from './wake-rows-helpers.js';
import { awaitTurnCompleted, plainScenario, setScenario, type ProviderName } from './thread-tools-helpers.js';
import type { MockInfo, MockSessionConfig } from './workflows-helpers.js';

const OWNS_SENTENCE = 'You own that directory and keep your durable state there.';

/** A mock's launch: what it was configured with, and Claude's `--resume` value. */
type SessionLaunch = MockSessionConfig & { resumeRef: string };

/**
 * The session configs of the mocks spawned in cwd, in spawn order, once
 * there are at least `expected` of them.
 */
async function configsIn(harness: HarnessApp, cwd: string, expected: number): Promise<SessionLaunch[]> {
  let seen: SessionLaunch[] = [];
  await expect
    .poll(
      async () => {
        const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
        seen = mocks
          .filter((mock) => mock.registration.cwd === cwd && mock.sessionConfig)
          .sort((a, b) => Number(a.mockId.split('-')[1]) - Number(b.mockId.split('-')[1]))
          .map((mock) => ({ ...(mock.sessionConfig as MockSessionConfig), resumeRef: mock.registration.resumeRef ?? '' }));
        return seen.length;
      },
      { timeout: 20_000 },
    )
    .toBeGreaterThanOrEqual(expected);
  return seen;
}

/** What the model reads beside its own prompt: the appended prompt on Claude, developer instructions on Codex. */
function appended(provider: ProviderName, config: MockSessionConfig): string {
  return (provider === 'claude' ? config.appendSystemPrompt : config.developerInstructions) ?? '';
}

async function runSession(harness: HarnessApp, thread: string, text: string): Promise<void> {
  const done = awaitTurnCompleted(harness, thread);
  await harness.rpc('SendMessage', thread, text, null);
  await done;
  await harness.rpc('StopSession', thread);
}

function row(page: Page, id: string) {
  return page.locator(`[data-sidebar-thread-id="${id}"]`);
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a standing agent's sessions carry its home on start and resume, and a demoted thread's do not`, async ({
    harness,
    page,
  }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `agent-home-${provider}`, repo: {}, threads: [{ title: 'Release agent', provider }] }],
    });
    const { path: cwd, threadIds } = seed.projects[0];
    const [thread] = threadIds;
    await setScenario(harness, cwd, plainScenario({ name: `home-${provider}`, provider, texts: ['ok'], afterTurns: 'repeatLast' }));
    await harness.rpc('GetProviderStatuses');

    // ---- Promotion makes the home and its README.
    const agent = await harness.rpc<{ home: string; threadId: string }>('PromoteStandingAgent', thread);
    expect(agent.home.endsWith(join('agents', thread))).toBe(true);
    const readme = readFileSync(join(agent.home, 'README.md'), 'utf8');
    expect(readme).toContain('# Release agent');
    expect(readme).toContain(`Thread id: ${thread}`);
    expect(readme).toContain(`agent-home-${provider}`);
    expect(readme).toContain('thread_watch');

    // With the guide off, nothing else is appended: the block stands alone.
    await harness.rpc('UpdateSettings', { agentGuideEnabled: false });
    try {
      // ---- A first start carries the block.
      await runSession(harness, thread, 'first');
      const [started] = await configsIn(harness, cwd, 1);
      const block = appended(provider, started);
      expect(block).toContain(agent.home);
      expect(block).toContain(thread);
      expect(block).toContain(OWNS_SENTENCE);
      if (provider === 'codex') expect(started.resumed ?? false).toBe(false);
      else expect(started.resumeRef).toBe('');

      // ---- A resume carries it again.
      await runSession(harness, thread, 'second');
      const resumed = (await configsIn(harness, cwd, 2))[1];
      if (provider === 'codex') expect(resumed.resumed).toBe(true);
      else expect(resumed.resumeRef).not.toBe('');
      expect(appended(provider, resumed)).toContain(agent.home);
      expect(appended(provider, resumed)).toContain(OWNS_SENTENCE);

      // ---- Demoted: the next session carries nothing, and the home stays.
      await harness.rpc('DemoteStandingAgent', thread);
      await runSession(harness, thread, 'third');
      const demoted = (await configsIn(harness, cwd, 3))[2];
      expect(appended(provider, demoted)).not.toContain(agent.home);
      expect(appended(provider, demoted)).not.toContain(OWNS_SENTENCE);
      expect(existsSync(join(agent.home, 'README.md'))).toBe(true);
    } finally {
      await harness.rpc('UpdateSettings', { agentGuideEnabled: true });
    }

    // ---- Promoted again, the kept home is reused; the Demote confirm's
    // destructive choice deletes it and leaves an ordinary row.
    const again = await harness.rpc<{ home: string }>('PromoteStandingAgent', thread);
    expect(again.home).toBe(agent.home);
    await harness.open(page);
    const agentRow = row(page, thread);
    await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Demote Standing Agent' }).click();
    const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
    await expect(dialog).toContainText('Demote keeps its home directory');
    await dialog.getByRole('button', { name: 'Demote and Delete Home' }).click();
    await expect(agentRow).not.toHaveAttribute('data-sidebar-agent', '');
    await expect.poll(() => existsSync(agent.home)).toBe(false);
    expect(await harness.rpc('GetStandingAgent', thread)).toBeNull();
  });
}

test('Demote and Delete Home whose home cannot be deleted still demotes and says why', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'agent-home-locked', repo: {}, threads: [{ title: 'Release agent', turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
  });
  const [thread] = seed.projects[0].threadIds;
  const agent = await harness.rpc<{ home: string }>('PromoteStandingAgent', thread);
  // A folder this user cannot list: removing the home fails inside it.
  const locked = join(agent.home, 'locked');
  mkdirSync(join(locked, 'inner'), { recursive: true });
  chmodSync(locked, 0o000);
  try {
    await harness.open(page);
    const agentRow = row(page, thread);
    await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Demote Standing Agent' }).click();
    const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
    await expect(dialog).toContainText(
      'This thread becomes an ordinary thread. Its watches are cancelled; its loops stay as they are. Demote keeps its home directory; Demote and Delete Home removes it.',
    );
    await capture(page, 'demote-dialog.png', dialog);
    await dialog.getByRole('button', { name: 'Demote and Delete Home' }).click();

    const toast = page.getByTestId('toast').filter({ hasText: 'The thread was demoted, but its home folder was not deleted:' });
    await expect(toast).toBeVisible();
    await toast.getByTestId('error-details-toggle').click();
    await expect(toast.getByTestId('toast-message')).toHaveText(
      'The thread was demoted, but its home folder was not deleted: permission denied.',
    );
    await expect(toast.getByTestId('error-details')).toContainText(agent.home);
    await expect(toast.getByTestId('error-details')).toContainText(join(agent.home, 'locked'));
    await capture(page, 'demote-delete-home-failed.png', toast);
    await expect(agentRow).not.toHaveAttribute('data-sidebar-agent', '');
    expect(await harness.rpc('GetStandingAgent', thread)).toBeNull();
    expect(existsSync(agent.home)).toBe(true);
  } finally {
    chmodSync(locked, 0o700);
  }
});
