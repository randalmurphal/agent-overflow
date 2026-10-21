// Creating an assistant (docs/specs/standing-agents.md, "Standing agent" and
// "Sidebar"), end to end on both mock providers.
//
// Coverage: New Assistant on the Assistants header opens the dialog with
// no project chosen, and from a project's menu with that project checked;
// Create makes the record, the home with its README and the thread outside
// every project, titled with the name, which opens in the pane; the
// app-written opening message reaches the provider in the home and shows
// as an agent row from Agent Overflow; the card lists in the Assistants
// section naming the projects it serves, and its Projects dialog changes
// them; a name another assistant holds, in any case, is refused inline
// naming it, from the dialog and from an inline rename, and nothing is
// created or renamed; promotion and demotion are offered nowhere and their
// bindings are gone.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { assistantsSection, cardShell, projectRow } from './sidebar-sections-helpers.js';
import { awaitMockInput, plainScenario, setScenario, threadRows, type ProviderName } from './thread-tools-helpers.js';

const MODEL: Record<ProviderName, string> = { claude: 'claude-opus-4-7', codex: 'gpt-5.2-codex' };

/** The next input a mock takes whose text includes `contains`, with the workspace it ran in. */
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
      projects: [{ name: `assistant-create-${provider}`, repo: {}, threads: [{ title: 'Ordinary', provider, turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
    });
    const { projectId, threadIds } = seed.projects[0];
    const projectName = `assistant-create-${provider}`;
    // A new thread takes the provider and model used last.
    await harness.rpc('CreateThread', { projectId, provider, model: MODEL[provider], mode: 'chat' });
    // The assistant runs in a home that does not exist yet: any workspace.
    await setScenario(harness, '', plainScenario({ name: `assistant-create-${provider}`, provider, texts: ['I am Ops. What am I responsible for?'], afterTurns: 'repeatLast', echoUserMessage: true }));

    await harness.open(page);
    const assistants = assistantsSection(page);
    await assistants.getByRole('button', { name: 'New Assistant' }).click();
    const dialog = page.getByTestId('new-assistant-dialog');
    await expect(dialog).toBeVisible();
    const projectBox = dialog.locator(`[data-testid="assistant-projects-item"][data-project-id="${projectId}"]`);
    await expect(projectBox).not.toBeChecked();
    await page.getByTestId('new-assistant-name').fill('Ops');
    await expect(page.getByTestId('new-assistant-create')).toBeDisabled();
    await projectBox.check();
    await captureAgentMessage(page, `${provider}-new-assistant-dialog.png`, page.getByRole('dialog'));
    const opening = awaitMockInput(harness, { includes: 'You are "Ops", a new standing agent the user just created' });
    await page.getByTestId('new-assistant-create').click();
    await expect(dialog).toHaveCount(0);

    // The thread opens in the pane, titled with the name, as an agent row.
    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const rows = await threadRows(harness);
    const created = rows.find((row) => row.title === 'Ops');
    expect(created, JSON.stringify(rows)).toBeDefined();
    expect(created!.provider).toBe(provider);
    const thread = created!.id;
    // Its card is in the Assistants section, naming the project it serves.
    const agentRow = assistants.locator(`[data-sidebar-agent][data-sidebar-thread-id="${thread}"]`);
    await expect(agentRow).toBeVisible();
    // The projects line sits under the row, in its shell.
    const projectsLine = cardShell(page, thread).getByTestId('thread-row-agent-projects');
    await expect(projectsLine).toHaveText(projectName);
    await expect(projectRow(page, thread)).toHaveCount(0);
    // No branch or worktree: the thread has no checkout.
    await expect(page.getByTestId('branch-picker-trigger')).toHaveCount(0);
    await expect(page.getByTestId('env-picker-trigger')).toHaveCount(0);

    // The record, the home with its README, and the thread outside every project.
    const agent = await harness.rpc<{ id: string; home: string; threadId: string; creatorAgentId?: string }>('GetStandingAgent', thread);
    expect(agent.threadId).toBe(thread);
    expect(agent.creatorAgentId ?? '').toBe('');
    expect(agent.home.endsWith(join('agents', agent.id))).toBe(true);
    expect(readFileSync(join(agent.home, 'README.md'), 'utf8')).toContain('# Ops');
    const row = await harness.rpc<{ projectId?: string; workspacePath: string }>('GetThread', thread);
    expect(row.projectId ?? '').toBe('');
    expect(row.workspacePath).toBe(agent.home);

    // The opening reached the provider in the home and shows as an agent row from the app.
    const { cwd, report } = await opening;
    const input = report.input ?? '';
    expect(cwd).toBe(agent.home);
    expect(input).toContain(`Your home is ${agent.home}, empty but for a README.`);
    const openingRow = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="opening"]');
    await expect(openingRow).toHaveCount(1);
    await expect(openingRow.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
    await expect(page.getByText('I am Ops. What am I responsible for?')).toBeVisible();
    await captureAgentMessage(page, `${provider}-assistant-opening.png`, openingRow);

    // ---- Its Projects dialog serves every project instead.
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Projects…' }).click();
    const projectsDialog = page.getByTestId('assistant-projects-dialog');
    await expect(projectsDialog.locator(`[data-testid="assistant-projects-item"][data-project-id="${projectId}"]`)).toBeChecked();
    await projectsDialog.getByTestId('assistant-projects-all').check();
    await captureAgentMessage(page, `${provider}-assistant-projects-dialog.png`, page.getByRole('dialog'));
    await page.getByTestId('assistant-projects-save').click();
    await expect(projectsDialog).toHaveCount(0);
    await expect(projectsLine).toHaveText('All projects');
    await captureAgentMessage(page, `${provider}-assistants-section.png`, assistants);

    // ---- A name in use, in another case, is refused inline from the dialog,
    // opened from the project's menu with that project checked.
    const project = page.getByTestId('project-item').filter({ hasText: projectName });
    await project.getByTestId('project-item-label').first().click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'New Assistant…' }).click();
    await expect(page.locator(`[data-testid="assistant-projects-item"][data-project-id="${projectId}"]`)).toBeChecked();
    await page.getByTestId('new-assistant-name').fill('ops');
    await page.getByTestId('new-assistant-create').click();
    await expect(page.getByTestId('new-assistant-error')).toHaveText('The assistant "Ops" already has that name; names are unique on this computer.');
    await expect(page.getByTestId('new-assistant-dialog')).toBeVisible();
    await captureAgentMessage(page, `${provider}-new-assistant-name-taken.png`, page.getByRole('dialog'));
    await page.getByTestId('new-assistant-cancel').click();
    expect((await threadRows(harness)).filter((r) => r.title.toLowerCase() === 'ops')).toHaveLength(1);

    // ---- And from an inline rename of another assistant.
    const scout = await harness.rpc<{ id: string }>('CreateAssistant', { name: 'Scout', projectIds: [projectId], allProjects: false });
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
