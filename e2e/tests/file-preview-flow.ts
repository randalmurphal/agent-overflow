// Real generated documents run at a separate origin, with relative assets,
// reload, session revocation and service-worker refusal on both layouts.
import { expect, test } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen, solePairedDevice } from './offhost-helpers.js';
import { COMPACT_SURFACE, DESKTOP_SURFACE, recordOpens } from './preview-gateway-helpers.js';

export function filePreviewFlow(): void {
  test('generated HTML opens from its computer with assets and isolated browser state', async ({ page, browser }, info) => {
    test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired browser');
    test.setTimeout(60_000);
    const harness = await launchHarness();
    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    const preview = await context.newPage();
    const surface = info.project.name === 'compact' ? COMPACT_SURFACE : DESKTOP_SURFACE;
    try {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
        name: 'generated-pages',
        repo: { commits: [{ message: 'generated site', files: {
          'pages/report.html': '<!doctype html><html><head><link rel="stylesheet" href="../assets/style.css"></head><body><h1>Report</h1><p id="script">waiting</p><script src="../assets/main.js"></script></body></html>',
          'assets/style.css': 'h1 { color: rgb(10, 20, 30); }',
          'assets/main.js': 'document.querySelector("#script").textContent = "Script ready";',
          'sw.js': 'self.addEventListener("fetch", () => {});',
          '.private.html': 'secret',
        } }] },
        threads: [{ title: 'Generated report', provider: 'claude', turns: [{ userText: 'Make a report', items: [{ kind: 'assistant_text', summary: '[Open report](pages/report.html) and [source](assets/main.js)' }] }] }],
      }] });
      const workspace = seed.projects[0].path;

      // On-host previews need no LAN, tailnet or installed certificate.
      const localURL = await harness.rpc<string>('MintFilePreviewURL', 'pages/report.html', workspace);
      expect(new URL(localURL).hostname).toBe('127.0.0.1');
      await preview.goto(localURL);
      await expect(preview.getByText('Script ready', { exact: true })).toBeVisible();
      await expect(preview.locator('h1')).toHaveCSS('color', 'rgb(10, 20, 30)');
      expect(new URL(preview.url()).search).toBe('');
      const worker = await preview.evaluate(async () => {
        try { await navigator.serviceWorker.register('/sw.js'); return 'registered'; }
        catch { return 'refused'; }
      });
      expect(worker).toBe('refused');
      expect(await preview.evaluate(async () => (await fetch('/.private.html')).status)).toBe(404);
      await preview.reload();
      await expect(preview.getByText('Script ready', { exact: true })).toBeVisible();

      await harness.rpc('SetNetworkSettings', { bindAll: true });
      const opens = await recordOpens(page);
      await confirmOnHost(harness, await redeemOnScreen(page, await mintInvite(harness, 'full'), 'HTML preview phone'));
      await expect(page.getByTestId('thread-row')).toHaveCount(1);
      await surface.openThread(page, 'Generated report');
      await expect(page.getByRole('link', { name: 'source', exact: true })).toHaveCount(0);
      await page.getByRole('link', { name: 'Open report', exact: true }).click();
      await expect.poll(() => opens.urls.length).toBe(1);
      const remoteURL = new URL(opens.urls[0]);
      expect(remoteURL.protocol).toBe('https:');
      expect(remoteURL.hostname).not.toBe('127.0.0.1');
      expect(remoteURL.origin).not.toBe(new URL(page.url()).origin);
      await preview.goto(remoteURL.href);
      await expect(preview.getByText('Script ready', { exact: true })).toBeVisible();
      await expect(preview.locator('h1')).toHaveCSS('color', 'rgb(10, 20, 30)');
      expect(await preview.evaluate(() => Object.keys(localStorage))).toEqual([]);
      await harness.rpc('SetNetworkSettings', { bindAll: false });
      await expect(preview.reload()).rejects.toThrow();
      // The same change must leave on-host pages usable.
      await preview.goto(localURL);
      await expect(preview.getByText('Script ready', { exact: true })).toBeVisible();
      await harness.rpc('SetNetworkSettings', { bindAll: true });
      await page.reload();
      await surface.openThread(page, 'Generated report');
      await page.getByRole('link', { name: 'Open report', exact: true }).click();
      await expect.poll(() => opens.urls.length).toBe(2);
      await preview.goto(opens.urls[1]);
      await expect(preview.getByText('Script ready', { exact: true })).toBeVisible();
      const device = await solePairedDevice(harness);
      await harness.rpc('RevokeAccessDevice', device.id);
      const refused = await preview.reload();
      expect(refused?.status()).toBe(401);
      await expect(preview.locator('body')).toContainText('This preview session ended.');
    } finally {
      await page.goto('about:blank');
      await context.close();
      await harness.close();
    }
  });
}
