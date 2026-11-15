// Provider CLI updates, end to end through the real backend and SPA:
//
//   announce - a release host that advertises a newer Claude Code and Codex
//              raises one toast per provider and a dot on each provider's
//              settings entry.
//   notes    - the toast's "What's new" opens the release notes between the
//              installed and the latest version, newest first, without the
//              prerelease in between.
//   update   - "Update to X" on the provider page runs the CLI's own update
//              command; the page, the dot and GetProviderUpdates follow the
//              version the binary reports afterwards.
//   reset    - the mock installs exactly the advertised version, and
//              HarnessReset restores its baseline install, so the next
//              seed is offered again.
//
// The spec owns its backend and a private copy of ao-mockprovider: an
// update and its reset move the mock executable's mtime, which every
// backend sharing that file would read as a provider upgrade. The copy
// sits in a Codex installer release directory, so Codex has an updater
// (`codex update` pinned to the latest GitHub release).
import { copyFile, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';

interface ProviderUpdateStatus {
  provider: string;
  installedVersion: string;
  latestVersion: string;
  available: boolean;
  checkError?: string;
  run: { phase: string; fromVersion?: string; toVersion?: string; error?: string };
}

const INSTALLED = '99.0.0';
const INTERMEDIATE = '99.0.1';
const LATEST = '99.1.0';

function sharedMockProvider(): string {
  return process.env.AO_MOCKPROVIDER
    ?? path.join(path.resolve(import.meta.dirname, '..', '..'), 'bin', 'ao-mockprovider');
}

function statusOf(statuses: ProviderUpdateStatus[], provider: string): ProviderUpdateStatus {
  const status = statuses.find((entry) => entry.provider === provider);
  expect(status, `${provider} must be in the provider update list`).toBeTruthy();
  return status!;
}

test('a newer provider CLI is announced, explained and installed from settings', async ({ page }) => {
  test.setTimeout(120_000);
  const root = await mkdtemp(path.join(tmpdir(), 'ao-provider-update-'));
  const release = path.join(root, '.codex', 'packages', 'standalone', 'releases', `${INSTALLED}-mock`);
  await mkdir(release, { recursive: true });
  const mockProvider = path.join(release, 'ao-mockprovider');
  await copyFile(sharedMockProvider(), mockProvider);
  let host: HarnessApp | undefined;
  try {
    host = await launchHarness({ mockProvider });

    // The default release hosts advertise the mock's own version.
    const before = await host.rpc<ProviderUpdateStatus[]>('CheckProviderUpdates');
    for (const provider of ['claude', 'codex']) {
      const status = statusOf(before, provider);
      expect(status.checkError ?? '').toBe('');
      expect(status.installedVersion).toBe(INSTALLED);
      expect(status.available).toBe(false);
    }

    await host.open(page);
    await expect(page.getByTestId('sidebar-settings-button')).toBeVisible();
    await expect(page.getByTestId('toast-message').filter({ hasText: 'is available' })).toHaveCount(0);

    // announce
    await host.rpc('HarnessSetProviderUpdateFeed', { latest: LATEST });
    const offered = await host.rpc<ProviderUpdateStatus[]>('CheckProviderUpdates');
    expect(statusOf(offered, 'claude')).toMatchObject({ latestVersion: LATEST, available: true });
    expect(statusOf(offered, 'codex')).toMatchObject({ latestVersion: LATEST, available: true });
    const claudeToast = page.getByTestId('toast').filter({ hasText: `Claude ${LATEST} is available` });
    await expect(claudeToast).toHaveCount(1);
    await expect(page.getByTestId('toast').filter({ hasText: `Codex ${LATEST} is available` })).toHaveCount(1);

    // A second check of the same version announces nothing again.
    await host.rpc('CheckProviderUpdates');
    await expect(claudeToast).toHaveCount(1);

    // notes
    await claudeToast.getByRole('button', { name: "What's new" }).click();
    const modal = page.getByTestId('provider-update-notes-modal');
    await expect(modal).toBeVisible();
    await expect(modal).toContainText(`${INSTALLED} to ${LATEST}`);
    const versions = modal.getByTestId('provider-update-notes-version');
    await expect(versions).toHaveCount(2);
    expect(await versions.evaluateAll((sections) => sections.map((s) => (s as HTMLElement).dataset.version)))
      .toEqual([LATEST, '99.0.1']);
    await expect(versions.first().getByTestId('provider-update-notes-body')).toContainText(`Changes in ${LATEST}.`);
    await expect(versions.nth(1).getByTestId('provider-update-notes-body')).toHaveCount(0);
    await versions.nth(1).getByRole('button').click();
    await expect(versions.nth(1).getByTestId('provider-update-notes-body')).toContainText('Changes in 99.0.1.');
    await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click();
    await expect(modal).toHaveCount(0);

    // update
    await page.getByTestId('sidebar-settings-button').click();
    const dot = (provider: string) =>
      page.locator(`[data-testid="settings-rail-provider-update-dot"][data-provider="${provider}"]`);
    await expect(dot('claude')).toBeVisible();
    await expect(dot('codex')).toBeVisible();
    await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: /^Claude Code/ }).click();
    const pill = page.getByTestId('settings-provider-status-pill');
    await expect(pill).toContainText(INSTALLED);
    const update = page.getByTestId('settings-provider-update-button');
    await expect(update).toHaveText(`Update to ${LATEST}`);
    await update.click();

    await expect(pill).toContainText(LATEST, { timeout: 30_000 });
    await expect(update).toHaveCount(0);
    await expect(page.getByTestId('settings-provider-update-error')).toHaveCount(0);
    await expect(dot('claude')).toHaveCount(0);
    await expect(page.getByTestId('toast-message').filter({ hasText: `Claude updated to ${LATEST}` })).toBeVisible();

    const after = statusOf(await host.rpc<ProviderUpdateStatus[]>('GetProviderUpdates'), 'claude');
    expect(after).toMatchObject({
      installedVersion: LATEST,
      available: false,
      run: { phase: 'succeeded', fromVersion: INSTALLED, toVersion: LATEST },
    });

    // Nothing newer is left, so a second run is refused inside the API.
    await expect(host.rpc('RunProviderUpdate', 'claude')).rejects.toThrow(/no newer version/);

    // reset
    const restored = async () => {
      await host!.rpc('HarnessReset');
      const statuses = await host!.rpc<ProviderUpdateStatus[]>('GetProviderUpdates');
      for (const provider of ['claude', 'codex']) {
        expect(statusOf(statuses, provider)).toMatchObject({ installedVersion: INSTALLED, available: false });
      }
    };
    const offeredAgain = async (version: string) => {
      await host!.rpc('HarnessSetProviderUpdateFeed', { latest: version });
      const statuses = await host!.rpc<ProviderUpdateStatus[]>('CheckProviderUpdates');
      for (const provider of ['claude', 'codex']) {
        expect(statusOf(statuses, provider)).toMatchObject({
          installedVersion: INSTALLED, latestVersion: version, available: true,
        });
      }
    };
    await restored();
    await offeredAgain(INTERMEDIATE);
    await host.rpc('RunProviderUpdate', 'codex');
    await expect.poll(async () => statusOf(await host!.rpc<ProviderUpdateStatus[]>('GetProviderUpdates'), 'codex').run, {
      timeout: 30_000,
    }).toMatchObject({ phase: 'succeeded', fromVersion: INSTALLED, toVersion: INTERMEDIATE });
    await restored();
    await offeredAgain(INTERMEDIATE);
  } finally {
    await host?.close();
    await rm(root, { recursive: true, force: true });
  }
});
