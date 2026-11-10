// Live dictation on the phone: the caption floats above the composer card
// in the card's box and the input's text column, and moves none of the
// composer's controls.

import { expect, test, type SeedResult } from './fixtures.js';
import { composerBoxes, expectCaptionInComposerGrid, writeOpenAIKey } from './dictation-helpers.js';

test.use({
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

test('the live caption sits in the composer grid on the phone', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'live-dictation-phone',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: 'Caption here', turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  await writeOpenAIKey(harness);
  await harness.rpc('UpdateSettings', { dictationSubscriptionEnabled: false, dictationAPIEnabled: true });
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  const mic = page.getByTestId('composer-dictate');
  await expect(mic).toBeVisible();
  await page.getByLabel('Message Input').fill('First line\nsecond line');
  const before = await composerBoxes(page);

  await mic.tap();
  await expect(mic).toHaveAttribute('data-phase', 'recording');
  await expect(page.getByTestId('composer-dictation-caption-text')).toContainText('Hearing', { timeout: 15_000 });
  await expectCaptionInComposerGrid(page, before);
  await mic.tap();
  await expect(mic).toHaveAttribute('data-phase', 'idle');
  await expect(page.getByTestId('composer-dictation-caption')).toHaveCount(0);
});
