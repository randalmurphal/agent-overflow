// Creating an assistant (docs/specs/standing-agents.md, "Standing agent" and
// "Sidebar"), end to end on both mock providers.
//
// Coverage: New Assistant on the project header opens the dialog on that
// project; Create makes the record, the home with its README and the
// thread, titled with the name, which opens in the pane; the app-written
// opening message reaches the provider and shows as an agent row from
// Agent Overflow; the row is an agent row; a name another assistant holds,
// in any case, is refused inline naming it, from the dialog and from an
// inline rename, and nothing is created or renamed; promotion and demotion
// are offered nowhere and their bindings are gone.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { plainScenario, setScenario, threadRows, type ProviderName } from './thread-tools-helpers.js';

const MODEL: Record<ProviderName, string> = { claude: 'claude-opus-4-7', codex: 'gpt-5.2-codex' };

/** The next input a mock in cwd takes whose text includes `contains`. */
async function inputIn(harness: HarnessApp, cwd: string, contains: string): Promise<string> {
  const event = await harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(contains),
    60_000,
  );
  return event.report.input ?? '';
}

async function menuLabels(page: Page, target: ReturnType<Page['locator']>): Promise<string[]> {
  await target.click({ button: 'right' });
  const labels = await page.getByRole('menuitem').evaluateAll((els) => els.map((el) => (el.textContent ?? '').trim()));
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menuitem')).toHaveCount(0);
  return labels;
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: New Assistant creates the record, the home and the thread, opens the role discussion, and refuses a name in use`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `assistant-create-${provider}`, repo: {}, threads: [{ title: 'Ordinary', provider }] }],
    });
    const { projectId, path: cwd, threadIds } = seed.projects[0];
    // A new thread takes the provider and model used last.
    await harness.rpc('CreateThread', { projectId, provider, model: MODEL[provider], mode: 'chat' });
    await setScenario(harness, cwd, plainScenario({ name: `assistant-create-${provider}`, provider, texts: ['I am Ops. What am I responsible for?'], afterTurns: 'repeatLast' }));

    await harness.open(page);
    const project = page.getByTestId('project-item').filter({ hasText: `assistant-create-${provider}` });
    await project.hover();
    await project.getByTestId('project-item-new-assistant').click();
    const dialog = page.getByTestId('new-assistant-dialog');
    await expect(dialog).toBeVisible();
    await expect(page.getByTestId('new-assistant-project')).toHaveValue(projectId);
    await page.getByTestId('new-assistant-name').fill('Ops');
    await expect(page.getByTestId('new-assistant-workspace')).toHaveValue('root');
    await captureAgentMessage(page, `${provider}-new-assistant-dialog.png`, page.getByRole('dialog'));
    const opening = inputIn(harness, cwd, 'You are "Ops", a new standing agent the user just created');
    await page.getByTestId('new-assistant-create').click();
    await expect(dialog).toHaveCount(0);

    // The thread opens in the pane, titled with the name, as an agent row.
    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const rows = await threadRows(harness);
    const created = rows.find((row) => row.title === 'Ops');
    expect(created, JSON.stringify(rows)).toBeDefined();
    expect(created!.provider).toBe(provider);
    const thread = created!.id;
    const agentRow = page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${thread}"]`);
    await expect(agentRow).toBeVisible();

    // The record and the home with its README.
    const agent = await harness.rpc<{ id: string; home: string; threadId: string; creatorAgentId?: string }>('GetStandingAgent', thread);
    expect(agent.threadId).toBe(thread);
    expect(agent.creatorAgentId ?? '').toBe('');
    expect(agent.home.endsWith(join('agents', agent.id))).toBe(true);
    expect(readFileSync(join(agent.home, 'README.md'), 'utf8')).toContain('# Ops');

    // The opening reached the provider and shows as an agent row from the app.
    const input = await opening;
    expect(input).toContain(`Your home is ${agent.home}, empty but for a README.`);
    const row = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="opening"]');
    await expect(row).toHaveCount(1);
    await expect(row.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
    await expect(page.getByText('I am Ops. What am I responsible for?')).toBeVisible();
    await captureAgentMessage(page, `${provider}-assistant-opening.png`, row);

    // ---- A name in use, in another case, is refused inline from the dialog.
    await project.hover();
    await project.getByTestId('project-item-new-assistant').click();
    await page.getByTestId('new-assistant-name').fill('ops');
    await page.getByTestId('new-assistant-create').click();
    await expect(page.getByTestId('new-assistant-error')).toHaveText('The assistant "Ops" already has that name; names are unique on this computer.');
    await expect(page.getByTestId('new-assistant-dialog')).toBeVisible();
    await captureAgentMessage(page, `${provider}-new-assistant-name-taken.png`, page.getByRole('dialog'));
    await page.getByTestId('new-assistant-cancel').click();
    expect((await threadRows(harness)).filter((r) => r.title.toLowerCase() === 'ops')).toHaveLength(1);

    // ---- And from an inline rename of another assistant.
    const scout = await harness.rpc<{ id: string }>('CreateAssistant', { projectId, name: 'Scout' });
    const scoutRow = page.locator(`[data-sidebar-thread-id="${scout.id}"]`);
    await expect(scoutRow).toHaveAttribute('data-sidebar-agent', '');
    await scoutRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Rename Thread' }).click();
    const renameInput = page.getByRole('textbox', { name: 'Rename Thread' });
    await renameInput.fill('OPS');
    await renameInput.press('Enter');
    await expect(page.getByTestId('thread-row-rename-error')).toHaveText('The assistant "Ops" already has that name; names are unique on this computer.');
    await captureAgentMessage(page, `${provider}-assistant-rename-taken.png`, scoutRow, page.getByTestId('thread-row-rename-error'));
    await renameInput.press('Escape');
    await expect.poll(async () => (await threadRows(harness)).find((r) => r.id === scout.id)?.title).toBe('Scout');

    // ---- Promotion and demotion are gone, from the menus and the bindings.
    for (const target of [page.locator(`[data-sidebar-thread-id="${threadIds[0]}"]`), agentRow]) {
      const labels = await menuLabels(page, target);
      expect(labels.some((label) => /Promote|Demote/.test(label)), labels.join(', ')).toBe(false);
    }
    for (const method of ['PromoteStandingAgent', 'DemoteStandingAgent', 'DemoteStandingAgentAndDeleteHome']) {
      const refusal = await harness.rpc(method, threadIds[0]).then(() => null, (err: Error) => err.message);
      expect(refusal, method).not.toBeNull();
    }
    expect(existsSync(agent.home)).toBe(true);
  });
}
