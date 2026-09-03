// The first turn of a "+ New" draft renders LIVE in the pane that sent it.
//
// Every other spec seeds a thread over RPC and opens it, which takes the
// mount path, and the mount path states the watched-thread set before the
// history loads go out. The in-app draft is the one path that does not
// mount: the pane holds a synthetic placeholder, `CreateThread` runs on the
// first keystroke or send, and the pane ADOPTS the real row in place. The
// backend narrows `provider:item_event` to the watched set, so a pane that
// adopts without restating that set gets the turn's status and none of its
// items until something unrelated restates it (2026-09-03: every new thread
// rendered nothing until a reload). This is the spec that failed before
// that fix and the one that keeps the draft path honest.
//
// The reply text is the claude default scenario's (`streaming-text`), so a
// harness with no scenario set answers exactly this.
import { test, expect, type SeedResult } from './fixtures.js';

test('the first turn of a "+ New" draft renders live in the pane that sent it', async ({
  harness,
  page,
}) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'draft-first-turn', repo: {} }],
  });

  await harness.open(page);
  await page.getByTestId('project-item-new-thread').first().click();
  await page.getByLabel('Message Input').fill('Start this thread');
  await page.getByTestId('composer-send').click();

  // Both rows come off the wire as items of the materialized thread: the
  // user echo first, then the streamed reply. Neither is asserted after a
  // reload, because a reload is exactly what this bug needed.
  await expect(page.getByTestId('user-message-summary')).toContainText('Start this thread');
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByTestId('assistant-message-body')).toContainText(
    'This reply is streaming from the mock provider',
  );
});
