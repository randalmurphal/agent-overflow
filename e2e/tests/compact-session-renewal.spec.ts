import { test } from '@playwright/test';
import { recoverRenewalAfterLostReply } from './session-renewal-flow.js';

test('a phone viewport recovers a committed renewal after losing its reply and restarting', async ({ browser, context, page }) => {
  await recoverRenewalAfterLostReply(browser, context, page);
});
