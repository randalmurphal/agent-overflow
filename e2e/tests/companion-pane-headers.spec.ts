// A thread companion beside its source pane on a 1280px screen, on both mock
// providers
// (docs/specs/standing-agents.md, "Agents segment"; docs/specs/side-chat.md):
// an agent thread companion and a side chat each open into the room the
// strip has left, so the strip does not scroll, and both pane headers keep
// their titles readable: a long title truncates with an ellipsis at an 8rem
// floor, and nothing in the header overflows the pane.
import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { agentRow, openAgents, seedLeadWithWorker } from './agents-tray-helpers.js';
import { awaitTurnCompleted, plainScenario, setScenario, type ProviderName } from './thread-tools-helpers.js';
import { LEAD, WORKER, expectHeadersShareTheWidth, expectStripFits, pane } from './companion-pane-headers-helpers.js';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an agent thread companion fits beside its source at 1280 with both titles readable`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const { token } = await seedLeadWithWorker(harness, { tag: `companion-headers-${provider}`, provider, lead: LEAD, worker: WORKER });
    await page.setViewportSize({ width: 1280, height: 720 });
    await harness.open(page);
    await openAgents(page, LEAD);
    await agentRow(page, token).getByTestId('agent-tray-row-open').click();
    await expect(pane(page, 'agent-thread').getByTestId('chat-header-title')).toHaveText(WORKER);

    await expectStripFits(page);
    await expectHeadersShareTheWidth([pane(page, 'thread'), pane(page, 'agent-thread')]);
    await captureAgentMessage(page, `${provider}-companion-headers-1280.png`, page.locator('section[data-pane-id]').first(), pane(page, 'agent-thread'));
  });

  test(`${provider}: a side chat fits beside its source at 1280 with both titles readable`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `side-chat-headers-${provider}`, repo: {}, threads: [{ title: LEAD, provider, turns: [{ userText: 'start the migration', items: [{ kind: 'assistant_text', summary: 'Started.' }] }] }] }],
    });
    const { path: cwd, threadIds } = seed.projects[0];
    await setScenario(harness, cwd, plainScenario({ name: `side-chat-headers-${provider}`, provider, texts: ['Noted.'], afterTurns: 'repeatLast', echoUserMessage: true }));
    await page.setViewportSize({ width: 1280, height: 720 });
    await harness.open(page);
    await page.locator(`[data-sidebar-thread-id="${threadIds[0]}"]`).click();
    const source = pane(page, 'thread');
    const turn = awaitTurnCompleted(harness, threadIds[0]);
    await source.getByLabel('Message Input').fill('What is next?');
    await source.getByTestId('composer-send').click();
    await turn;
    await source.getByLabel('Message Input').fill('/side-chat');
    await source.getByTestId('composer-send').click();
    await expect(pane(page, 'side-chat')).toHaveCount(1);
    await expect(pane(page, 'side-chat').getByTestId('side-chat-preparing')).toHaveCount(0);
    await expect(pane(page, 'side-chat').getByTestId('chat-header-title')).toContainText('Side chat');

    await expectStripFits(page);
    await expectHeadersShareTheWidth([source, pane(page, 'side-chat')]);
    await captureAgentMessage(page, `${provider}-side-chat-headers-1280.png`, source, pane(page, 'side-chat'));
  });
}
