import { test, expect } from './base-test.js';
import { COMPUTER_READ_DEADLINE_MS } from '../../frontend/src/lib/pageTiming.ts';
import { launchHarness } from '../src/harness.js';
import { holdPageClock } from '../src/page-clock.js';
import { seedAgentThread } from './agent-visibility-helpers.js';
import { PAIRED_APP_MOUNT_MS, confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen, socketAnswers } from './offhost-helpers.js';
import { awaitReconnectArmed, reconnectArmed, runNextReconnect, watchProjectsQuiet } from './sidebar-quiet-helpers.js';

export function offlineStartupFlow(): void {
  test('a paired frontend opens offline without toast spam and restores its thread on reconnect', async ({ page }) => {
    // Room for the reconnect's wait (PAIRED_REDIAL_MS) on top of the rest.
    test.setTimeout(90_000);
    test.skip(nonLoopbackIPv4() === null, 'requires a non-loopback interface for the paired peer');
    const harness = await launchHarness();
    const exceptions: string[] = [];
    page.on('pageerror', (error) => exceptions.push(error.message));
    let online = true;
    let capture = false;
    const toasts: string[] = [];
    await page.exposeFunction('__recordStartupToast', (text: string) => { if (capture) toasts.push(text); });
    // The page clock runs as the real one does until the outage has outlived
    // the boot reads. It is held then, and the reconnect ladder's attempts
    // are run one at a time by moving it on. Installed before the deadline
    // tracker below, which wraps the clock's timers.
    await page.clock.install();
    await page.addInitScript((readDeadlineMs) => {
      // Boot reads race the startup read budget (COMPUTER_READ_DEADLINE_MS,
      // for the catalog rows and the UI state, both through
      // readBeforeDeadline). Track every timer of that length so the outage
      // can end once each has fired or been cleared, instead of after a
      // fixed sleep.
      const deadlines = { armed: 0, fired: 0, pending: new Set<unknown>() };
      (window as unknown as { __bootDeadlines: typeof deadlines }).__bootDeadlines = deadlines;
      const nativeSet = window.setTimeout.bind(window);
      const nativeClear = window.clearTimeout.bind(window);
      window.setTimeout = ((handler: TimerHandler, delay?: number, ...args: unknown[]) => {
        if (delay !== readDeadlineMs || typeof handler !== 'function') return nativeSet(handler, delay, ...args);
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
    }, COMPUTER_READ_DEADLINE_MS);
    const answers = socketAnswers();
    let refused = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      if (!online) { refused += 1; void socket.close({ code: 1012, reason: 'test offline launch' }); return; }
      answers.connect(socket);
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
      // The asleep computer lists the tree its last complete read kept.
      await expect(page.getByTestId('thread-row')).toHaveCount(1);
      // From an armed attempt on, the page dials only when the spec runs
      // the clock to it.
      const clock = await holdPageClock(page);
      await awaitReconnectArmed(clock);
      // The banner says the computer is unreachable and carries Retry, so
      // the Projects section adds no line and runs nothing through the
      // ladder's next two attempts, each refused by the route.
      expect(await watchProjectsQuiet(page, async () => {
        for (let attempt = 0; attempt < 2; attempt++) {
          const dialed = refused;
          await runNextReconnect(clock);
          expect(refused, 'the ladder dialed other than once in a run of the clock').toBe(dialed + 1);
          // The watch ends after the page, not the route, has handled the refusal.
          expect(await reconnectArmed(page), 'the pass ended before the page armed its next attempt').toBe(true);
        }
      })).toMatchObject({ statusLines: [], animations: [] });
      await expect(page.getByTestId('transport-status-retry')).toBeVisible();
      expect(toasts).toEqual([]);
      const answered = answers.count();
      online = true;
      // The banner goes once the backend answers the ladder's next dial.
      await runNextReconnect(clock);
      await page.clock.resume();
      await answers.waitBeyond(answered);
      await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);
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
