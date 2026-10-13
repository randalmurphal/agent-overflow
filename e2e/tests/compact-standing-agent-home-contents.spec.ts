// A standing agent's home contents under the compact layout
// (docs/specs/standing-agents.md, "Agent home contents"): the same flow as
// standing-agent-home-contents.spec.ts on the Claude mock, then the agent's
// session, its hygiene wake with the pass's outcome and the worker's first
// message with the attached file, each fitting the phone.
import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import { ATTACHED_OPENING, HYGIENE_LABEL, HYGIENE_OUTCOME, VERIFICATION, runHomeContents } from './standing-agent-home-contents-helpers.js';

test('compact: the agent session, its hygiene wake and the worker first message fit the phone', async ({ harness, page }) => {
  test.setTimeout(240_000);
  await runHomeContents(harness, 'claude', 'compact-home-contents');

  await harness.open(page);
  await page.getByTestId('thread-row').getByText('Release lead', { exact: true }).tap();
  const wake = page
    .getByTestId('user-message-bubble')
    .filter({ has: page.getByTestId('user-message-wake-origin').getByText(`Loop · ${HYGIENE_LABEL}`, { exact: true }) });
  await expect(wake).toHaveCount(1);
  await expect(page.getByText(HYGIENE_OUTCOME)).toBeVisible();
  await expect(page.getByText('Your standing instructions')).toHaveCount(0);
  const opening = page.getByTestId('user-message-bubble').filter({ hasText: 'Set up your home.' });
  await expect(opening).toHaveCount(1);
  await opening.scrollIntoViewIfNeeded();
  await capture(page, 'compact-agent-session.png', opening);
  await wake.scrollIntoViewIfNeeded();
  await capture(page, 'compact-hygiene-wake.png', wake);

  await page.goBack();
  await page.getByTestId('thread-row').getByText('Parser review', { exact: true }).tap();
  const first = page.getByTestId('user-message-bubble').filter({ hasText: ATTACHED_OPENING });
  await expect(first).toHaveCount(1);
  await expect(first).toContainText(VERIFICATION);
  await first.scrollIntoViewIfNeeded();
  await capture(page, 'compact-worker-first-message.png', page.getByTestId('user-message-bubble').filter({ hasText: ATTACHED_OPENING }).getByText(ATTACHED_OPENING));
});
