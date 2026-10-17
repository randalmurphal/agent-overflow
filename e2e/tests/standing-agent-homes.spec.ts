// An assistant's home (docs/specs/standing-agents.md, "Standing agent"),
// end to end on both mock providers.
//
// Coverage: an assistant's home is `<config>/agents/<assistant-id>/` with a
// README naming the assistant, its id, its first thread and its project;
// the home block (home path, assistant id, thread id, the sentence that the
// agent owns the directory) reaches the provider on a start and on a
// resume, as Claude's appended system prompt and as Codex's developer
// instructions, with the app guide switched off so the block cannot ride on
// it; on Codex, a rewrite of instructions.md between two sessions reaches
// the resumed thread as one injected developer message, and an unchanged
// home injects nothing; deleting the assistant's thread is confirmed once,
// whatever the confirmDelete setting, naming the home folder, and removes
// the home with the record; and a home that cannot be removed leaves the delete
// unfinished with a toast saying why and the folder named in its details.
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
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

/** The mocks spawned in cwd, in spawn order, once there are at least `expected` of them. */
async function mocksIn(harness: HarnessApp, cwd: string, expected: number): Promise<MockInfo[]> {
  let seen: MockInfo[] = [];
  await expect
    .poll(
      async () => {
        const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
        seen = mocks
          .filter((mock) => mock.registration.cwd === cwd && mock.sessionConfig)
          .sort((a, b) => Number(a.mockId.split('-')[1]) - Number(b.mockId.split('-')[1]));
        return seen.length;
      },
      { timeout: 20_000 },
    )
    .toBeGreaterThanOrEqual(expected);
  return seen;
}

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
  test(`${provider}: an assistant's sessions carry its home on start and resume, and deleting it removes the home`, async ({
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

    // ---- The record makes the home and its README.
    const agent = await harness.rpc<{ id: string; home: string }>('HarnessMakeAssistant', thread);
    expect(agent.home.endsWith(join('agents', agent.id))).toBe(true);
    const readme = readFileSync(join(agent.home, 'README.md'), 'utf8');
    expect(readme).toContain('# Release agent');
    expect(readme).toContain(`- Assistant id: ${agent.id}`);
    expect(readme).toContain(`- First thread: ${thread}`);
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
      expect(block).toContain(`assistant id ${agent.id}`);
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
    } finally {
      await harness.rpc('UpdateSettings', { agentGuideEnabled: true });
    }

    // ---- Delete, with confirmDelete off: one confirm naming the home, and
    // the home goes with the record.
    await harness.rpc('UpdateSettings', { confirmDelete: false });
    await harness.open(page);
    const agentRow = row(page, thread);
    await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Delete', exact: true }).click();
    const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
    await expect(dialog).toContainText(`The assistant "Release agent" is deleted too, and its home folder ${agent.home} is removed with everything in it.`);
    await capture(page, `${provider}-assistant-delete-dialog.png`, dialog);
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(agentRow).toHaveCount(0);
    await expect.poll(() => existsSync(agent.home)).toBe(false);
    expect(await harness.rpc('GetStandingAgent', thread)).toBeNull();
  });
}

// A resumed Codex thread restores its context from the rollout and never
// shows a changed developerInstructions to the model, so the home block's
// change reaches it only as an injected developer message. Each session is
// its own mock process, so a mock's latched injectedItems is that
// session's alone.
test("codex: a rewrite of an assistant's instructions.md reaches its resumed thread once", async ({ harness }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'agent-instructions-codex', repo: {}, threads: [{ title: 'Release agent', provider: 'codex' }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const [thread] = threadIds;
  await setScenario(harness, cwd, plainScenario({ name: 'instructions-codex', provider: 'codex', texts: ['ok'], afterTurns: 'repeatLast' }));
  await harness.rpc('GetProviderStatuses');
  const agent = await harness.rpc<{ id: string; home: string }>('HarnessMakeAssistant', thread);
  const instructions = join(agent.home, 'instructions.md');
  const BEFORE = 'Carry parser changes from review to merge.';
  const AFTER = 'Carry release notes from draft to publication.';
  writeFileSync(instructions, `${BEFORE}\n`);

  // ---- A fresh start carries the instructions and injects nothing.
  await runSession(harness, thread, 'first');
  const [started] = await mocksIn(harness, cwd, 1);
  expect(started.sessionConfig?.resumed ?? false).toBe(false);
  expect(started.sessionConfig?.developerInstructions).toContain(BEFORE);
  expect(started.injectedItems ?? []).toEqual([]);

  // ---- The rewrite reaches the resumed thread as one developer message
  // that carries the new text and not the old.
  writeFileSync(instructions, `${AFTER}\n`);
  await runSession(harness, thread, 'second');
  const resumed = (await mocksIn(harness, cwd, 2))[1];
  expect(resumed.sessionConfig?.resumed).toBe(true);
  const injected = resumed.injectedItems ?? [];
  expect(injected).toHaveLength(1);
  expect(injected[0].role).toBe('developer');
  expect(injected[0].text.startsWith('The developer instructions for this conversation have changed.')).toBe(true);
  expect(injected[0].text).toContain(AFTER);
  expect(injected[0].text).not.toContain(BEFORE);
  expect(injected[0].text).toContain(OWNS_SENTENCE);

  // ---- An unchanged home on the next resume injects nothing.
  await runSession(harness, thread, 'third');
  const unchanged = (await mocksIn(harness, cwd, 3))[2];
  expect(unchanged.sessionConfig?.resumed).toBe(true);
  expect(unchanged.injectedItems ?? []).toEqual([]);
});

test('an assistant whose home cannot be removed is not deleted, and the toast says why', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'agent-home-locked', repo: {}, threads: [{ title: 'Release agent', turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
  });
  const [thread] = seed.projects[0].threadIds;
  const agent = await harness.rpc<{ home: string }>('HarnessMakeAssistant', thread);
  // A folder this user cannot list: removing the home fails inside it.
  const locked = join(agent.home, 'locked');
  mkdirSync(join(locked, 'inner'), { recursive: true });
  chmodSync(locked, 0o000);
  try {
    await harness.open(page);
    const agentRow = row(page, thread);
    await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Delete', exact: true }).click();
    const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click();

    // The toast's own line names the folder that is still there.
    const toast = page.getByTestId('toast').filter({ hasText: `the assistant's home folder ${agent.home} could not be removed` });
    await expect(toast).toBeVisible();
    await toast.getByTestId('error-details-toggle').click();
    await expect(toast.getByTestId('error-details')).toContainText(agent.home);
    await capture(page, 'assistant-delete-home-failed.png', toast);
    expect(await harness.rpc('GetStandingAgent', thread)).not.toBeNull();
    expect(existsSync(agent.home)).toBe(true);
  } finally {
    chmodSync(locked, 0o700);
  }
});
