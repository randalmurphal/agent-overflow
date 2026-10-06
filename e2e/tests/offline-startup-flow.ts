import { test, expect } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { seedAgentThread } from './agent-visibility-helpers.js';
import { PAIRED_APP_MOUNT_MS, confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';

export function offlineStartupFlow(): void {
  test('a paired frontend opens offline without toast spam and restores its thread on reconnect', async ({ page }) => {
    test.setTimeout(60_000);
    test.skip(nonLoopbackIPv4() === null, 'requires a non-loopback interface for the paired peer');
    const harness = await launchHarness();
    const exceptions: string[] = [];
    page.on('pageerror', (error) => exceptions.push(error.message));
    let online = true;
    let capture = false;
    const toasts: string[] = [];
    await page.exposeFunction('__recordStartupToast', (text: string) => { if (capture) toasts.push(text); });
    await page.addInitScript(() => {
      // Boot reads race a 2.5s deadline (COMPUTER_READ_DEADLINE_MS and the
      // UI-state read, both through readBeforeDeadline). Track every timer
      // of that length so the outage can end once each has fired or been
      // cleared, instead of after a fixed sleep.
      const deadlines = { armed: 0, fired: 0, pending: new Set<unknown>() };
      (window as unknown as { __bootDeadlines: typeof deadlines }).__bootDeadlines = deadlines;
      const nativeSet = window.setTimeout.bind(window);
      const nativeClear = window.clearTimeout.bind(window);
      window.setTimeout = ((handler: TimerHandler, delay?: number, ...args: unknown[]) => {
        if (delay !== 2500 || typeof handler !== 'function') return nativeSet(handler, delay, ...args);
        deadlines.armed++;
        const id = nativeSet(() => {
          deadlines.pending.delete(id);
          deadlines.fired++;
          (handler as (...a: unknown[]) => void)(...args);
        }, delay);
        deadlines.pending.add(id);
        return id;
      }) as typeof window.setTimeout;
      window.clearTimeout = ((id?: number) => {
        deadlines.pending.delete(id);
        nativeClear(id);
      }) as typeof window.clearTimeout;
      const seen = new Set<string>();
      new MutationObserver(() => {
        for (const element of document.querySelectorAll('[data-testid="toast"]')) {
          const text = element.textContent ?? '';
          if (seen.has(text)) continue;
          seen.add(text);
          (window as unknown as { __recordStartupToast(text: string): void }).__recordStartupToast(text);
        }
      }).observe(document, { subtree: true, childList: true });
    });
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      if (online) socket.connectToServer();
      else void socket.close({ code: 1012, reason: 'test offline launch' });
    });
    try {
      await harness.rpc('SetNetworkSettings', { bindAll: true });
      const threadId = await seedAgentThread(harness, 'offline-startup', 'Resume this conversation');
      await harness.rpc('SaveDraft', threadId, 'Keep this draft', [], [], null);
      const invite = await mintInvite(harness, 'full');
      await confirmOnHost(harness, await redeemOnScreen(page, invite, 'Offline startup client'));
      await expect(page.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });
      await page.getByTestId('thread-row').click();
      await expect(page.getByText('Ready.', { exact: true })).toBeVisible();
      await expect(page.getByLabel('Message Input')).toHaveValue('Keep this draft');
      online = false;
      capture = true;
      await page.reload();
      await expect(page.getByTestId('transport-status-banner')).toBeVisible();
      // Keep the outage past every boot read's deadline.
      await page.waitForFunction(() => {
        const deadlines = (window as unknown as { __bootDeadlines: { fired: number; pending: Set<unknown> } }).__bootDeadlines;
        return deadlines.fired > 0 && deadlines.pending.size === 0;
      });
      expect(toasts).toEqual([]);
      online = true;
      await expect(page.getByTestId('transport-status-banner')).toHaveCount(0, { timeout: 20_000 });
      await expect(page.getByText('Ready.', { exact: true })).toBeVisible();
      await expect(page.getByLabel('Message Input')).toBeEnabled();
      await expect(page.getByLabel('Message Input')).toHaveValue('Keep this draft');
      expect(toasts).toEqual([]);
      expect(exceptions).toEqual([]);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
