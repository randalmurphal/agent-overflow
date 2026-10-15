// Thread companion headers under the compact layout
// (docs/specs/standing-agents.md, "Agents segment"; docs/specs/side-chat.md):
// an agent thread companion and a side chat each fill the phone, and the
// header of each keeps its title readable (whole, or at the 8rem floor and
// fading) with nothing running past the screen.
import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { agentRow, openAgents } from './agents-tray-helpers.js';
import { awaitTurnCompleted, plainScenario, setScenario } from './thread-tools-helpers.js';
import { LEAD, WORKER, expectHeadersShareTheWidth, pane, seedLeadWithWorker } from './companion-pane-headers-helpers.js';

test('compact: an agent thread companion header keeps its title readable', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const { token } = await seedLeadWithWorker(harness, 'compact-companion-headers');
  await harness.open(page);
  await openAgents(page, LEAD);
  await agentRow(page, token).getByTestId('agent-tray-row-open').tap();
  const companion = pane(page, 'agent-thread');
  await expect(companion.getByTestId('chat-header-title')).toHaveText(WORKER);
  await expect(companion.getByTestId('chat-header-title')).toBeInViewport();

  await expectHeadersShareTheWidth([companion]);
  await captureAgentMessage(page, 'compact-companion-headers.png', companion.getByTestId('chat-header'), companion.getByTestId('chat-header'));
});

test('compact: a side chat header keeps its title readable', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-side-chat-headers', repo: {}, threads: [{ title: LEAD, provider: 'claude', turns: [{ userText: 'start the migration', items: [{ kind: 'assistant_text', summary: 'Started.' }] }] }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  await setScenario(harness, cwd, plainScenario({ name: 'compact-side-chat-headers', provider: 'claude', texts: ['Noted.'], afterTurns: 'repeatLast', echoUserMessage: true }));
  await harness.open(page);
  await page.locator(`[data-sidebar-thread-id="${threadIds[0]}"]`).tap();
  const source = pane(page, 'thread');
  await expectHeadersShareTheWidth([source]);
  const turn = awaitTurnCompleted(harness, threadIds[0]);
  await source.getByLabel('Message Input').fill('What is next?');
  await source.getByTestId('composer-send').tap();
  await turn;
  await source.getByLabel('Message Input').fill('/side-chat');
  await source.getByTestId('composer-send').tap();
  const sideChat = pane(page, 'side-chat');
  await expect(sideChat.getByTestId('side-chat-preparing')).toHaveCount(0);
  await expect(sideChat.getByTestId('chat-header-title')).toContainText('Side chat');
  await expect(sideChat.getByTestId('chat-header-title')).toBeInViewport();

  await expectHeadersShareTheWidth([sideChat]);
  await captureAgentMessage(page, 'compact-side-chat-headers.png', sideChat.getByTestId('chat-header'), sideChat.getByTestId('chat-header'));
});
