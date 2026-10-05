// A build without remote access (`<meta name="ao-remote-access" content="off">`)
// offers no remote surface in Settings; the standard build offers all of
// them. Each case states both sides, so a guard that leaks into the
// standard build fails here as surely as one that is missing.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import SettingsView from './SettingsView.svelte';
import NotificationsSection from './NotificationsSection.svelte';
import UpdatesSettings from './UpdatesSettings.svelte';
import ThemeSettings from './ThemeSettings.svelte';
import { searchHitKey, searchSettings } from './settingsSearch';
import { SETTINGS_SECTIONS, settingsSectionAvailable } from './sections';
import { seedSettingsPages } from '../../../test/helpers/settingsPages';
import { stageNoRemoteBuild } from '../../../test/helpers/buildVariant';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import {
  getSettingsSection,
  isSettingsOpen,
  isSettingsRailOpen,
  openSettingsOverlay,
  resetSettingsOverlayForTest,
} from '../../stores/settingsOverlay.svelte';
import { getUpdateState, resetForTest as resetUpdatesForTest } from '../../stores/updates.svelte';
import { initServiceUpdates, resetServiceUpdatesForTest } from '../../stores/serviceUpdate.svelte';

const REMOTE_PAGES = ['systems', 'remote', 'agent-access'] as const;
const REMOTE_TABS = ['Connect to a computer', 'Allow device access', 'Agent remote tools'];

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
}

function keys(query: string): string[] {
  return searchSettings(query).map(searchHitKey);
}

const variants = [
  { name: 'standard build', remote: true },
  { name: 'build without remote access', remote: false },
];

for (const variant of variants) {
  describe(variant.name, () => {
    beforeEach(async () => {
      if (!variant.remote) stageNoRemoteBuild();
      resetSettingsOverlayForTest();
      await seedSettingsPages();
    });

    afterEach(() => {
      resetSettingsOverlayForTest();
      resetToLocalPage();
    });

    it(variant.remote ? 'lists the remote pages in the rail' : 'omits the remote pages and their group from the rail', () => {
      const view = render(SettingsView, { onClose: vi.fn() });
      for (const name of REMOTE_TABS) {
        expect(view.queryByRole('tab', { name }) !== null, name).toBe(variant.remote);
      }
      expect(view.queryByText('Remote access') !== null).toBe(variant.remote);
      expect(view.getAllByRole('tab')).toHaveLength(
        SETTINGS_SECTIONS.length - (variant.remote ? 0 : REMOTE_PAGES.length),
      );
    });

    it('moves keyboard focus through the pages the rail shows', async () => {
      const view = render(SettingsView, { onClose: vi.fn() });
      const editor = view.getByRole('tab', { name: 'Editor' });
      await fireEvent.click(editor);
      await fireEvent.keyDown(view.getByRole('tab', { name: 'Editor' }), { key: 'ArrowDown' });
      const next = variant.remote ? 'Connect to a computer' : 'Observability';
      expect(view.getByRole('tab', { name: next })).toHaveAttribute('aria-selected', 'true');
    });

    it(variant.remote ? 'finds remote controls in search' : 'finds no remote control or page in search', () => {
      if (variant.remote) {
        expect(keys('lan')).toContain('remote.allow-remote-access');
        expect(keys('tailnet')).toContain('remote.tailnet');
        expect(keys('paired')).toContain('notifications.phone-push');
        expect(keys('phone push')).toContain('notifications.phone-push');
        expect(keys('connect')).toContain('page:systems');
        return;
      }
      for (const query of ['lan', 'tailnet', 'pair', 'phone push', 'phone', 'connect', 'device access', 'remote', 'passkey']) {
        for (const hit of searchSettings(query)) {
          expect(settingsSectionAvailable(hit.page.id), `${query}: ${searchHitKey(hit)}`).toBe(true);
          if (hit.kind === 'field') expect(hit.field.remote ?? false, `${query}: ${hit.field.id}`).toBe(false);
        }
      }
      expect(keys('tailnet')).toEqual([]);
      expect(keys('phone push')).toEqual([]);
      // "pair" alone also matches "repair" in a local hint.
      expect(keys('paired')).toEqual([]);
      expect(keys('pairing')).toEqual([]);
      expect(keys('lan')).not.toContain('remote.allow-remote-access');
      expect(keys('connect')).not.toContain('page:systems');
    });

    it('shows no remote result in the rail search box', async () => {
      const view = render(SettingsView, { onClose: vi.fn() });
      await fireEvent.input(view.getByTestId('settings-search'), { target: { value: 'tailnet' } });
      await tick();
      expect(view.queryByText('Join my tailnet') !== null).toBe(variant.remote);
      if (!variant.remote) expect(view.getByText('No settings match.')).toBeInTheDocument();
    });

    it(variant.remote ? 'opens a remote page by deep link' : 'opens the default page for a remote deep link', () => {
      for (const page of REMOTE_PAGES) {
        resetSettingsOverlayForTest();
        openSettingsOverlay(page);
        expect(isSettingsOpen()).toBe(true);
        expect(getSettingsSection()).toBe(variant.remote ? page : 'theme');
        expect(isSettingsRailOpen()).toBe(!variant.remote);
      }
      openSettingsOverlay('git');
      expect(getSettingsSection()).toBe('git');
    });

    it(variant.remote ? 'names the computer a computer page configures' : 'names no computer on a computer page', async () => {
      const view = render(SettingsView, { onClose: vi.fn() });
      await fireEvent.click(view.getByRole('tab', { name: 'Notifications' }));
      const header = view.getByTestId('settings-page-header');
      expect(header.textContent?.includes('Computer:')).toBe(variant.remote);
      expect(header.textContent?.includes('phone push')).toBe(variant.remote);
      expect(header.textContent).toContain('Desktop alerts');
    });

    it('shows the version without a variant label', async () => {
      const view = render(SettingsView, { onClose: vi.fn() });
      const footer = await waitFor(() => view.getByText(/Agent Overflow v0\.0\.1/));
      expect(footer.textContent?.trim()).toBe('Agent Overflow v0.0.1');
    });

    it(variant.remote ? 'offers phone push on the notifications page' : 'offers no phone push on the notifications page', async () => {
      const { container } = render(NotificationsSection);
      await settle();
      expect(container.querySelector('[data-settings-field="notifications.phone-push"]') !== null).toBe(variant.remote);
      expect(container.textContent?.includes('A paired phone is still woken.')).toBe(variant.remote);
      expect(container.textContent).toContain('Held back on this screen only.');
    });

    describe('updates page', () => {
      beforeEach(() => {
        resetUpdatesForTest();
        resetServiceUpdatesForTest();
      });
      afterEach(() => resetServiceUpdatesForTest());

      it(variant.remote ? 'lists supervised computers' : 'lists no other computer', async () => {
        initServiceUpdates();
        const view = render(UpdatesSettings);
        emitWailsEvent('service:update-status', {
          supervised: true, available: true, currentVersion: '1.2.0', phase: 'idle',
        });
        await settle();
        expect(view.queryByTestId('machine-updates') !== null).toBe(variant.remote);
      });

      it('shows the current version without a variant label', () => {
        const s = getUpdateState();
        s.supported = true;
        s.currentVersion = '1.0.0';
        const view = render(UpdatesSettings);
        expect(view.getByText('1.0.0')).toBeInTheDocument();
      });
    });

    it(variant.remote ? 'offers copying themes from a computer to a paired device' : 'offers no copy from a computer', async () => {
      await pairWithScopes(['settings:read']);
      const { container } = render(ThemeSettings);
      await settle();
      expect(container.querySelector('[data-settings-field="theme.copy-files"]') !== null).toBe(variant.remote);
    });
  });
}
