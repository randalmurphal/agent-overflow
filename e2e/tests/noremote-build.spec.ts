// The build without remote access (docs/architecture/noremote-build.md),
// run as bin/agent-overflow-noremote from `make harness-build`. Covers: the
// served shell names the variant, Settings offers no remote page, the wire
// refuses remote-only calls and remote network settings, the backend listens
// on loopback, and a local conversation still runs. The standard binary is
// checked the same way so the hidden pages are known to exist there.
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';

const repoRoot = path.resolve(import.meta.dirname, '..', '..');
const binDir = path.dirname(process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow'));
const noremoteBinary = path.join(binDir, 'agent-overflow-noremote');
const standardBinary = path.join(binDir, 'agent-overflow');
const remotePages = ['Connect to a computer', 'Allow device access', 'Agent remote tools'];

async function settingsRail(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const overlay = page.getByTestId('settings-overlay');
  await expect(overlay.getByRole('tab', { name: 'Git', exact: true })).toBeVisible();
  return overlay;
}

test('the build without remote access hides and refuses remote access and keeps local work', async ({ page }) => {
  test.setTimeout(120_000);
  let host: HarnessApp | undefined;
  try {
    host = await launchHarness({ binary: noremoteBinary });
    expect(new URL(host.bootstrap.url).hostname).toBe('127.0.0.1');

    await expect(host.rpc('MintDevicePairing', 'phone', 'view')).rejects.toThrow('remote access is not available in this build');
    await expect(host.rpc('DiscoverComputers')).rejects.toThrow('remote access is not available in this build');
    const network = await host.rpc<Record<string, unknown>>('GetNetworkSettings');
    await expect(host.rpc('SetNetworkSettings', { ...network, bindAll: true })).rejects.toThrow('remote access is not available in this build');

    await host.open(page);
    await expect(page.locator('meta[name="ao-remote-access"]')).toHaveAttribute('content', 'off');
    const overlay = await settingsRail(page);
    for (const label of remotePages) {
      await expect(overlay.getByRole('tab', { name: label, exact: true })).toHaveCount(0);
    }
    await page.keyboard.press('Escape');

    await host.rpc('HarnessSetScenario', { name: 'thinking-then-text' });
    const seed = await host.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: 'Local project', repo: {}, threads: [{ title: 'Local conversation' }] }],
    });
    const threadId = seed.projects[0].threadIds[0];
    await host.rpc('StartSession', threadId);
    await host.rpc('SendMessage', threadId, 'local turn', null);
    await host.waitForEvent('provider:turn_completed');
    await page.getByTestId('thread-row').filter({ hasText: 'Local conversation' }).click();
    await expect(page.getByText('local turn', { exact: true })).toBeVisible();
  } finally {
    try {
      await page.goto('about:blank');
    } finally {
      await host?.close();
    }
  }
});

test('the standard build serves no variant marker and offers the remote pages', async ({ page }) => {
  let host: HarnessApp | undefined;
  try {
    host = await launchHarness({ binary: standardBinary });
    await host.open(page);
    await expect(page.locator('meta[name="ao-remote-access"]')).toHaveCount(0);
    const overlay = await settingsRail(page);
    for (const label of remotePages) {
      await expect(overlay.getByRole('tab', { name: label, exact: true })).toBeVisible();
    }
  } finally {
    try {
      await page.goto('about:blank');
    } finally {
      await host?.close();
    }
  }
});
