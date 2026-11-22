// A standing agent's home contents under the compact layout
// (docs/specs/standing-agents.md, "Agent home contents"): the same flow as
// standing-agent-home-contents.spec.ts on the Claude mock, then the agent's
// session and the worker's first message, an agent row opened on the
// attached file and the facts its footer carried, each fitting the phone;
// the banner on the assistant while it runs with its provider's own memory
// too (expectOwnMemoryBanner), shown and cleared on the assistant and never
// on its worker or an ordinary thread.
import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import {
  AGENT_SENTENCE,
  ATTACHED_OPENING,
  ROLE,
  VERIFICATION,
  expectOwnMemoryBanner,
  runHomeContents,
  scrollTimelineToTop,
  showTextUncovered,
} from './standing-agent-home-contents-helpers.js';
import { openRunsFor } from './standing-agent-wake-helpers.js';

test('compact: the agent session and the worker first message fit the phone', async ({ harness, page }) => {
  test.setTimeout(240_000);
  // Settings outlive a test in the worker. Collapsed runs keep the agent's
  // earlier prose inside the timeline's window on the phone.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'collapsed' });
  const run = await runHomeContents(harness, 'claude', 'compact-home-contents');

  await harness.open(page);
  await expect(page.getByLabel(/^Could not fully verify/)).toHaveCount(0);
  // The assistant's one row, in the Assistants section.
  await page.getByTestId('sidebar-assistants-section').getByTestId('thread-row').getByText('Release lead', { exact: true }).tap();
  await expect(page.getByText('Noted.', { exact: true })).toBeVisible();
  await expect(page.getByText('Your standing instructions')).toHaveCount(0);
  const opening = page.getByTestId('user-message-bubble').filter({ hasText: 'Set up your home.' });
  await scrollTimelineToTop(page, opening);
  await capture(page, 'compact-agent-session.png', opening);

  await page.goBack();
  // The worker is owned, with no sidebar row until it is shown there, as
  // Show in Sidebar on its Agents row does.
  await harness.rpc('SetThreadPlacement', [run.worker], 'standalone');
  await page.getByTestId('thread-row').getByText('Parser review', { exact: true }).tap();
  const first = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="spawn"]');
  await openRunsFor(page, first);
  await expect(first).toHaveCount(1);
  await expect(first.getByTestId('agent-message-first-line')).toHaveText(ATTACHED_OPENING);
  // Expanded, and scrolled to the facts naming the agent.
  await first.getByTestId('agent-message-toggle').tap();
  await expect(first.getByTestId('agent-message-text')).toContainText(VERIFICATION);
  await expect(first).not.toContainText(AGENT_SENTENCE);
  await expect(first.locator('[data-fact="Role"]')).toHaveText(ROLE);
  await showTextUncovered(page, first, 'Go to the sender, which spawned this thread');
  await capture(page, 'compact-worker-first-message.png');

  await expectOwnMemoryBanner(harness, page, 'claude', async (section, title) => {
    await page.goBack();
    await page.getByTestId(`sidebar-${section}-section`).getByTestId('thread-row').getByText(title, { exact: true }).tap();
  });
});
