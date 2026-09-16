// Mobile browser content locking in Chromium and WebKit. The backend, pairing,
// passkey signatures, grants, storage, touch interaction and navigation are real.
// Playwright supplies a software authenticator; OS credential dialogs are outside
// this suite. Background timing uses explicit lifecycle events because automation
// keeps pages active. Navigation also exercises browser-generated page lifecycle.
import type { BrowserContext, Page } from '@playwright/test';
import { test as base, expect } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { confirmOnHost, redeemOnScreen, nonLoopbackIPv4, type PairingInvite } from './offhost-helpers.js';
import { installSoftwarePasskeys, passkeyDomainProxy } from './passkey-helpers.js';
import { launchLifecycleBrowser } from './browser-lifecycle-helpers.js';

const LOCK_KEY = 'agent-overflow:browserLock';
const SESSION_KEY = 'agent-overflow:deviceSession';
const DOMAIN = 'ao.e2e.test';
const lanIP = nonLoopbackIPv4();

async function openLockSettings(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Allow device access', exact: true }).click();
}

async function pairLockedViewer(harness: HarnessApp, page: Page): Promise<void> {
  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'view-only');
  const pairedURL = new URL(invite.url);
  pairedURL.protocol = 'https:';
  pairedURL.hostname = DOMAIN;
  const payload = JSON.parse(Buffer.from(pairedURL.hash.slice('#pair='.length), 'base64url').toString());
  payload.endpoint = pairedURL.origin;
  pairedURL.hash = 'pair=' + Buffer.from(JSON.stringify(payload)).toString('base64url');
  const code = await redeemOnScreen(page, { ...invite, url: pairedURL.toString() }, 'Mobile browser');
  await confirmOnHost(harness, code);
  await expect(page.getByTestId('view-only-indicator')).toBeVisible();
  await openLockSettings(page);
  const toggle = page.getByRole('switch', { name: 'Require a passkey to open' });
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
}

const test = base.extend<{
  mobileHost: { harness: HarnessApp; owner: BrowserContext; proxy: { server: string }; setOffline: (value: boolean) => void };
  lockedBrowser: { harness: HarnessApp; page: Page };
}>({
  mobileHost: async ({ browser }, use) => {
    test.skip(lanIP === null, 'A non-loopback interface is required for a real remote browser session');
    const harness = await launchHarness();
    const cleanup: Array<() => Promise<void>> = [() => harness.close()];
    try {
      const localProxy = await passkeyDomainProxy(DOMAIN, harness.bootstrap.port, '127.0.0.1');
      cleanup.unshift(() => localProxy.close());
      const remoteProxy = await passkeyDomainProxy(DOMAIN, harness.bootstrap.port, lanIP!);
      cleanup.unshift(() => remoteProxy.close());
      const owner = await browser.newContext({ ignoreHTTPSErrors: true, proxy: { server: localProxy.server } });
      cleanup.unshift(() => owner.close());
      await harness.rpc('SetNetworkSettings', { bindAll: true, canonicalDomain: DOMAIN });
      await use({ harness, owner, proxy: { server: remoteProxy.server }, setOffline: remoteProxy.setOffline });
    } finally {
      const errors: unknown[] = [];
      for (const close of cleanup) {
        try { await close(); } catch (error) { errors.push(error); }
      }
      if (errors.length) throw new AggregateError(errors, 'Mobile browser fixture cleanup failed');
    }
  },
  proxy: async ({ mobileHost }, use) => { await use(mobileHost.proxy); },
  lockedBrowser: async ({ mobileHost, context, page }, use) => {
    const { harness, owner } = mobileHost;
    await installSoftwarePasskeys(owner);
    const host = await owner.newPage();
    const url = new URL(await harness.pageURL());
    url.protocol = 'https:';
    url.hostname = DOMAIN;
    await host.goto(url.toString());
    await host.getByTestId('sidebar-settings-button').click();
    await host.getByRole('tab', { name: 'Allow device access', exact: true }).click();
    await host.getByText('Security & passkeys', { exact: true }).click();
    await host.getByRole('button', { name: 'Add a passkey', exact: true }).click();
    await expect.poll(async () => (await harness.rpc<unknown[]>('ListPasskeys')).length, {
      message: 'the software authenticator registration must verify on the real backend',
    }).toBe(1);
    const credentials = await owner.credentials.get();
    expect(credentials).toHaveLength(1);
    await context.credentials.create(DOMAIN, credentials[0]);
    await installSoftwarePasskeys(context);

    await pairLockedViewer(harness, page);
    await use({ harness, page });
  },
});

test.use({ ignoreHTTPSErrors: true });

async function session(page: Page) {
  return page.evaluate((key) => JSON.parse(localStorage.getItem(key)!), SESSION_KEY);
}

async function unlock(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Unlock', exact: true }).tap();
  await expect(page.getByTestId('app-lock')).toHaveCount(0);
}

async function expectCovered(page: Page): Promise<void> {
  await expect(page.getByTestId('app-lock')).toBeVisible();
  await expect(page.locator('#app')).toHaveAttribute('inert', '');
  const button = await page.getByRole('button', { name: 'Unlock', exact: true }).boundingBox();
  expect(button).not.toBeNull();
  const viewport = page.viewportSize()!;
  expect(button!.x).toBeGreaterThanOrEqual(0);
  expect(button!.y).toBeGreaterThanOrEqual(0);
  expect(button!.x + button!.width).toBeLessThanOrEqual(viewport.width);
  expect(button!.y + button!.height).toBeLessThanOrEqual(viewport.height);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(viewport.width);
}

test('touch unlock preserves view-only access across tabs, reload and reopening', async ({ lockedBrowser, context }) => {
  const { page } = lockedBrowser;
  const before = await session(page);
  expect(before.scopes).not.toContain('threads:operate');
  const url = page.url();
  await page.reload();
  await expectCovered(page);
  const second = await context.newPage();
  await second.goto(url);
  await expectCovered(second);
  await unlock(second);
  await expectCovered(page);
  await page.setViewportSize({ width: 844, height: 390 });
  await expectCovered(page);
  await unlock(page);
  await expect(page.getByTestId('view-only-indicator')).toBeVisible();
  expect((await session(page)).sessionId).toBe(before.sessionId);
  expect((await session(page)).scopes).toEqual(before.scopes);
  await page.close();
  await second.close();
  const reopened = await context.newPage();
  await reopened.goto(url);
  await expectCovered(reopened);
  await unlock(reopened);
  expect((await session(reopened)).sessionId).toBe(before.sessionId);
});

test('background grace, canceled verification and returning through browser history stay locked when owed', async ({ lockedBrowser }) => {
  const { page } = lockedBrowser;
  const start = Date.now();
  await page.clock.setFixedTime(start);
  await page.reload();
  await unlock(page);
  const visibility = (hidden: boolean) => page.evaluate((value) => {
    Object.defineProperty(document, 'hidden', { configurable: true, value });
    document.dispatchEvent(new Event('visibilitychange'));
  }, hidden);
  await visibility(true);
  await expectCovered(page);
  await page.clock.setFixedTime(start + 4_000);
  await visibility(false);
  await expect(page.getByTestId('app-lock')).toHaveCount(0);
  await visibility(true);
  await page.clock.setFixedTime(start + 304_000);
  await visibility(false);
  await expectCovered(page);
  // Device request signatures use wall-clock timestamps checked by the backend.
  await page.clock.setFixedTime(Date.now());
  await page.evaluate(() => {
    const original = navigator.credentials.get.bind(navigator.credentials);
    navigator.credentials.get = async (...args) => {
      navigator.credentials.get = original;
      throw new DOMException('Canceled', 'NotAllowedError');
    };
  });
  await page.getByRole('button', { name: 'Unlock', exact: true }).tap();
  await expect(page.getByTestId('app-lock').getByRole('alert')).toContainText('Could not verify your passkey');
  await visibility(true);
  await visibility(false);
  await expectCovered(page);
  await unlock(page);
  // Restore the real property before navigating. Back may restore a cached
  // document or reload it; both must enter locked, using actual browser events.
  await page.evaluate(() => Reflect.deleteProperty(document, 'hidden'));
  const url = page.url();
  await page.goto('about:blank');
  await page.goBack();
  await expect(page).toHaveURL(url);
  await expectCovered(page);
  await unlock(page);
});

test('server rejection cannot reveal content; retry and disabling keep the original session', async ({ lockedBrowser }) => {
  const { page } = lockedBrowser;
  const before = await session(page);
  await page.reload();
  await expectCovered(page);
  // Corrupt a real signed assertion. This must reach and fail server verification,
  // rather than merely dismissing a mocked prompt in the frontend.
  await page.evaluate(() => {
    const original = navigator.credentials.get.bind(navigator.credentials);
    navigator.credentials.get = async (...args) => {
      navigator.credentials.get = original;
      const credential = await original(...args) as PublicKeyCredential;
      Object.defineProperty(credential.response, 'signature', { value: new Uint8Array([0]).buffer });
      return credential;
    };
  });
  await page.getByRole('button', { name: 'Unlock', exact: true }).tap();
  await expect(page.getByTestId('app-lock').getByRole('alert')).toContainText('Could not verify your passkey');
  await expectCovered(page);
  await unlock(page);
  const toggle = page.getByRole('switch', { name: 'Require a passkey to open' });
  if (!await toggle.isVisible()) await openLockSettings(page);
  await toggle.tap();
  await expect(toggle).toHaveAttribute('aria-checked', 'false');
  expect(await page.evaluate((key) => localStorage.getItem(key), LOCK_KEY)).toBeNull();
  await page.reload();
  await expect(page.getByTestId('view-only-indicator')).toBeVisible();
  await expect(page.getByTestId('app-lock')).toHaveCount(0);
  expect((await session(page)).sessionId).toBe(before.sessionId);
  expect((await session(page)).scopes).toEqual(before.scopes);
});

test('revoking a locked mobile browser cannot be bypassed with its passkey', async ({ lockedBrowser }) => {
  const { page, harness } = lockedBrowser;
  const before = await session(page);
  await page.reload();
  await expectCovered(page);
  await harness.rpc('RevokeAccessSession', before.sessionId);
  await expect(page.getByTestId('transport-status-banner')).toHaveAttribute('data-status', 'pairing-required');
  await page.getByRole('button', { name: 'Unlock', exact: true }).tap();
  await expectCovered(page);
  await expect(page.getByTestId('app-lock')).toContainText('open a new pairing link');
});

test('losing connectivity while locked preserves the cover and recovers the same session', async ({ lockedBrowser, mobileHost }) => {
  const { page } = lockedBrowser;
  const before = await session(page);
  await page.reload();
  await expectCovered(page);
  mobileHost.setOffline(true);
  try {
    await expect(page.getByTestId('transport-status-banner')).toHaveAttribute('data-status', /disconnected|reconnecting/);
    await page.getByRole('button', { name: 'Unlock', exact: true }).tap();
    await expectCovered(page);
  } finally { mobileHost.setOffline(false); }
  // An issued request may resume after reconnect or be refused promptly. Both
  // paths must keep the cover until a fresh assertion reaches this backend.
  await expect.poll(async () => {
    if (await page.getByTestId('app-lock').count() === 0) return 'verified';
    return await page.getByRole('button', { name: 'Unlock', exact: true }).isEnabled() ? 'retry' : 'pending';
  }).not.toBe('pending');
  if (await page.getByTestId('app-lock').count()) await unlock(page);
  await expect(page.getByTestId('view-only-indicator')).toBeVisible();
  expect((await session(page)).sessionId).toBe(before.sessionId);
  expect((await session(page)).scopes).toEqual(before.scopes);
});

test('browser-generated visibility covers immediately and enforces the background deadline', { tag: '@chromium-lifecycle' }, async ({ lockedBrowser, mobileHost, context }) => {
  const real = await launchLifecycleBrowser(mobileHost.proxy.server);
  try {
    const credentials = await context.credentials.get();
    await real.context.credentials.create(DOMAIN, credentials[0]);
    await installSoftwarePasskeys(real.context);
    const start = Date.now();
    await real.page.clock.setFixedTime(start);
    await pairLockedViewer(lockedBrowser.harness, real.page);
    expect(await real.page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
    const events: Array<{ hidden: boolean; trusted: boolean }> = [];
    await real.page.exposeFunction('__recordVisibility', (hidden: boolean, trusted: boolean) => events.push({ hidden, trusted }));
    await real.page.evaluate(() => document.addEventListener('visibilitychange', event => {
      const report = (window as unknown as { __recordVisibility: (hidden: boolean, trusted: boolean) => void }).__recordVisibility;
      report(document.hidden, event.isTrusted);
    }));
    const other = await real.context.newPage();
    await other.goto('about:blank');
    await other.bringToFront();
    await expect.poll(() => real.page.evaluate(() => document.hidden)).toBe(true);
    await expectCovered(real.page);
    await real.page.clock.setFixedTime(start + 4_000);
    await real.page.bringToFront();
    await expect(real.page.getByTestId('app-lock')).toHaveCount(0);
    await other.bringToFront();
    await expect.poll(() => real.page.evaluate(() => document.hidden)).toBe(true);
    await expectCovered(real.page);
    await real.page.clock.setFixedTime(start + 304_000);
    await real.page.bringToFront();
    await expect.poll(() => real.page.evaluate(() => document.hidden)).toBe(false);
    await expectCovered(real.page);
    await real.page.clock.setFixedTime(Date.now());
    await real.page.getByRole('button', { name: 'Unlock', exact: true }).click();
    await expect(real.page.getByTestId('app-lock')).toHaveCount(0);
    await expect.poll(() => events.map(event => event.hidden)).toEqual([true, false, true, false]);
    expect(events.every(event => event.trusted)).toBe(true);
  } finally { await real.close(); }
});
