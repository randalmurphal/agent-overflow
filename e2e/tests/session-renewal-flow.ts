import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';

export async function recoverRenewalAfterLostReply(browser: Browser, context: BrowserContext, page: Page): Promise<void> {
  expect(nonLoopbackIPv4(), 'this regression requires an off-host LAN address').toBeTruthy();
  const harness = await launchHarness();
  let restarted: BrowserContext | undefined;
  try {
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    await harness.rpc('HarnessSeed', { projects: [{ name: 'renewal-recovery', repo: {}, threads: [{ title: 'Survives renewal',
      turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }] });
    const invite = await mintInvite(harness, 'full');
    await confirmOnHost(harness, await redeemOnScreen(page, invite, 'Renewal browser'));
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    let committed = false;
    await context.route('**/auth/token/recover', async (route) => {
      if (!committed) {
        const result = await route.fetch({ maxRetries: 0 });
        expect(result.status()).toBe(200);
        committed = true;
        await result.dispose();
      }
      await route.abort('connectionreset');
    });
    await page.evaluate(() => {
      const key = 'agent-overflow:deviceSession';
      const held = JSON.parse(localStorage.getItem(key)!);
      if (held.refreshRecovery !== true) throw new Error('backend did not advertise renewal recovery');
      held.expiresAtMs = 1;
      localStorage.setItem(key, JSON.stringify(held));
    });
    await page.reload();
    await expect.poll(() => committed).toBe(true);
    await expect.poll(() => page.evaluate(() => {
      const held = JSON.parse(localStorage.getItem('agent-overflow:deviceSession') ?? 'null');
      return !!held?.pendingNextSecret;
    })).toBe(true);
    const saved = await context.storageState({ indexedDB: true });
    const origin = new URL(invite.url).origin;
    const held = JSON.parse(saved.origins.find((entry) => entry.origin === origin)!.localStorage.find((entry) => entry.name === 'agent-overflow:deviceSession')!.value);
    const viewport = page.viewportSize();
    await context.close();
    restarted = await browser.newContext({ storageState: saved, viewport, isMobile: !!viewport && viewport.width < 600, hasTouch: !!viewport && viewport.width < 600 });
    const resumed = await restarted.newPage();
    await resumed.goto(origin);
    await expect(resumed.getByTestId('thread-row')).toHaveCount(1);
    const recovered = await resumed.evaluate(() => {
      const session = JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!);
      return { sessionId: session.sessionId, refreshSecret: session.refreshSecret, pending: !!session.pendingNextSecret };
    });
    // Compare booleans so failed assertion output never prints a credential.
    expect(recovered.sessionId === held.sessionId).toBe(true);
    expect(recovered.refreshSecret === held.pendingNextSecret).toBe(true);
    expect(recovered.pending).toBe(false);
  } finally {
    await restarted?.close();
    await context.close();
    await harness.close();
  }
}
