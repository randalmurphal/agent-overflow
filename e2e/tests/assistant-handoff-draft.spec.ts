// New Thread, Same Assistant with unsent text in the assistant's composer,
// on both mock providers (docs/specs/standing-agents.md, "Next thread"): the
// pane follows the assistant to its new thread and its composer shows the
// text, which the new thread's draft holds and the past thread's no longer
// does. The button is pressed right after typing, before the composer's
// save would have run on its own.
import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { assistantHome, awaitTurnCompleted, plainScenario, setScenario, threadRows, type ProviderName } from './thread-tools-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: unsent text in the composer follows New Thread, Same Assistant`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `assistant-handoff-draft-${provider}`, repo: {}, threads: [{ title: 'Ops', provider, assistant: true }] }],
    });
    const [first] = seed.projects[0].threadIds;
    const cwd = await assistantHome(harness, first);
    await setScenario(harness, cwd, plainScenario({ name: `assistant-handoff-draft-${provider}`, provider, texts: ['Picked up.'], afterTurns: 'repeatLast', echoUserMessage: true }));
    const turn = awaitTurnCompleted(harness, first);
    await harness.rpc('SendMessage', first, 'hello', null);
    await turn;
    await harness.open(page);
    await page.locator(`[data-sidebar-thread-id="${first}"]`).click();
    const input = page.getByRole('textbox', { name: 'Message Input', exact: true });
    await input.click();
    await page.keyboard.type('half-typed note');
    await page.getByTestId('chat-header-next-thread').filter({ visible: true }).click();

    let second = '';
    await expect.poll(async () => {
      second = (await threadRows(harness)).find((row) => row.title === 'Ops' && row.id !== first && !row.archived)?.id ?? '';
      return second;
    }).not.toBe('');
    await expect(input).toHaveValue('half-typed note');
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', second)).content).toBe('half-typed note');
    expect((await harness.rpc<{ content: string }>('GetDraft', first)).content).toBe('');
    await captureAgentMessage(page, `${provider}-assistant-handoff-draft.png`, page.getByTestId('composer-root').first());
  });
}
