// Creating an assistant under the compact layout, on both mock providers
// (docs/specs/standing-agents.md, "Sidebar"): New Assistant is in the
// project's long-press menu, the dialog fits the phone, Create opens the
// assistant's thread screen with its opening message, and a name in use is
// refused inline.
import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { longPress } from './touch-helpers.js';
import { plainScenario, setScenario, threadRows, type ProviderName } from './thread-tools-helpers.js';

const MODEL: Record<ProviderName, string> = { claude: 'claude-opus-4-7', codex: 'gpt-5.2-codex' };

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: compact: New Assistant from the project menu opens the assistant with its opening message`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `compact-assistant-create-${provider}`, repo: {}, threads: [{ title: 'Ordinary', provider, turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
    });
    const { projectId } = seed.projects[0];
    // A new thread takes the provider and model used last.
    await harness.rpc('CreateThread', { projectId, provider, model: MODEL[provider], mode: 'chat' });
    // The assistant runs in a home that does not exist yet: any workspace.
    await setScenario(harness, '', plainScenario({ name: `compact-assistant-create-${provider}`, provider, texts: ['I am Ops. What am I responsible for?'], afterTurns: 'repeatLast', echoUserMessage: true }));
    await harness.open(page);

    const project = page.getByTestId('project-item').filter({ hasText: `compact-assistant-create-${provider}` });
    const item = page.getByRole('menuitem', { name: 'New Assistant…' });
    await longPress(page, project.getByTestId('project-item-label').first(), item);
    await item.tap();
    const dialog = page.getByRole('dialog');
    await expect(page.getByTestId('new-assistant-dialog')).toBeVisible();
    // Opened from the project's menu, the dialog serves that project.
    await expect(page.locator(`[data-testid="assistant-projects-item"][data-project-id="${projectId}"]`)).toBeChecked();
    await page.getByTestId('new-assistant-name').fill('Ops');
    const create = page.getByTestId('new-assistant-create');
    await expect(create).toBeInViewport({ ratio: 1 });
    await captureAgentMessage(page, `${provider}-compact-new-assistant-dialog.png`, dialog);
    await create.tap();

    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const row = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="opening"]');
    await expect(row).toHaveCount(1);
    await expect(row.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
    await expect(page.getByText('I am Ops. What am I responsible for?')).toBeVisible();
    await captureAgentMessage(page, `${provider}-compact-assistant-opening.png`, row);
    const created = (await threadRows(harness)).find((r) => r.title === 'Ops');
    expect(created).toBeDefined();
    expect((await harness.rpc<{ provider: string }>('GetThread', created!.id)).provider).toBe(provider);
    expect(await harness.rpc('GetStandingAgent', created!.id)).not.toBeNull();

    // A name in use is refused inline, in the dialog on the phone.
    await page.getByTestId('compact-back').tap();
    await longPress(page, project.getByTestId('project-item-label').first(), item);
    await item.tap();
    await page.getByTestId('new-assistant-name').fill('OPS');
    await page.getByTestId('new-assistant-create').tap();
    const error = page.getByTestId('new-assistant-error');
    await expect(error).toHaveText('The assistant "Ops" already has that name; names are unique on this computer.');
    await expect(error).toBeInViewport({ ratio: 1 });
    await captureAgentMessage(page, `${provider}-compact-new-assistant-name-taken.png`, dialog);
  });
}
