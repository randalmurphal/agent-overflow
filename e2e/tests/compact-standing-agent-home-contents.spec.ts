// A standing agent's home contents under the compact layout
// (docs/specs/standing-agents.md, "Agent home contents"): the same flow as
// standing-agent-home-contents.spec.ts on the Claude mock, then the agent's
// session, its hygiene wake with the pass's outcome and the worker's first
// message, an agent row opened on the attached file and the facts its
// footer carried, each fitting the phone.
import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import {
  AGENT_SENTENCE,
  ATTACHED_OPENING,
  HYGIENE_LABEL,
  HYGIENE_OUTCOME,
  ROLE,
  VERIFICATION,
  centerInTimeline,
  expectUncovered,
  runHomeContents,
  scrollTimelineToTop,
  showTextUncovered,
} from './standing-agent-home-contents-helpers.js';

test('compact: the agent session, its hygiene wake and the worker first message fit the phone', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runHomeContents(harness, 'claude', 'compact-home-contents');

  await harness.open(page);
  await expect(page.getByLabel(/^Could not fully verify/)).toHaveCount(0);
  await page.getByTestId('thread-row').getByText('Release lead', { exact: true }).tap();
  const wake = page
    .locator('[data-testid="agent-message-row"][data-agent-message-kind="loop"]')
    .filter({ has: page.getByTestId('agent-message-sender').getByText(HYGIENE_LABEL, { exact: true }) });
  await expect(wake).toHaveCount(1);
  await expect(page.getByText(HYGIENE_OUTCOME)).toBeVisible();
  await expect(page.getByText('Your standing instructions')).toHaveCount(0);
  const opening = page.getByTestId('user-message-bubble').filter({ hasText: 'Set up your home.' });
  await scrollTimelineToTop(page, opening);
  await capture(page, 'compact-agent-session.png', opening);
  // The pass's outcome under the wake, clear of the composer.
  const outcome = page.getByText(HYGIENE_OUTCOME);
  await centerInTimeline(outcome);
  await expectUncovered(outcome);
  await expectUncovered(wake.getByTestId('agent-message-toggle'));
  await capture(page, 'compact-hygiene-wake.png', wake.getByTestId('agent-message-toggle'), outcome);

  await page.goBack();
  await page.getByTestId('thread-row').getByText('Parser review', { exact: true }).tap();
  const first = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="spawn"]');
  await expect(first).toHaveCount(1);
  await expect(first.getByTestId('agent-message-first-line')).toHaveText(ATTACHED_OPENING);
  // Expanded, and scrolled to the facts naming the agent.
  await first.getByTestId('agent-message-toggle').tap();
  await expect(first.getByTestId('agent-message-text')).toContainText(VERIFICATION);
  await expect(first).not.toContainText(AGENT_SENTENCE);
  await expect(first.locator('[data-fact="Role"]')).toHaveText(ROLE);
  await showTextUncovered(page, first, 'Go to the sender, which spawned this thread');
  await capture(page, 'compact-worker-first-message.png');
});
