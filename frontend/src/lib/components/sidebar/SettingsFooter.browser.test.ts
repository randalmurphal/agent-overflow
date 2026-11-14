import { afterEach, beforeEach, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import '../../../app.css';
import SettingsFooter from './SettingsFooter.svelte';
import { SIDEBAR_MIN_WIDTH } from '../../stores/sidebarLayout.svelte';
import { getUpdateState, resetForTest as resetUpdatesForTest } from '../../stores/updates.svelte';
import { resetToLocalPage } from '../../../test/helpers/scopes';

// The footer is one row that never wraps. With the Update button in it the
// row is wider than the narrowest sidebar unless Settings gives up its label.

let host: HTMLDivElement;
beforeEach(() => {
  resetToLocalPage();
  resetUpdatesForTest();
  host = document.createElement('div');
  host.style.cssText = 'background:var(--color-surface-0);color:var(--color-fg)';
  document.body.append(host);
});
afterEach(() => {
  cleanup();
  host.remove();
  resetUpdatesForTest();
});

// Every control sits inside the footer, none overlaps the next, and no
// control's content spills out of it.
function expectRowFits(footer: HTMLElement): void {
  const row = footer.getBoundingClientRect();
  const controls = Array.from(footer.querySelectorAll<HTMLElement>('button'));
  let previousRight = row.left;
  for (const control of controls) {
    const rect = control.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(previousRight - 0.5);
    expect(rect.right).toBeLessThanOrEqual(row.right + 0.5);
    expect(control.scrollWidth).toBeLessThanOrEqual(control.clientWidth);
    previousRight = rect.right;
  }
}


it.each([SIDEBAR_MIN_WIDTH, 232, 256, 280, 360])(
  'fits Settings, Update and the toggles in a %ipx sidebar',
  async (width) => {
    host.style.width = `${width}px`;
    getUpdateState().latestVersion = '2.0.0';
    const view = render(SettingsFooter, { target: host, props: { onOpenSettings() {} } });

    const update = await view.findByTestId('sidebar-update-button');
    const settings = view.getByTestId('sidebar-settings-button');
    expect(view.getByTestId('sidebar-keep-awake-toggle')).toBeTruthy();
    expectRowFits(settings.parentElement!);
    expect(update.getBoundingClientRect().width).toBeGreaterThan(40);
    expect(settings).toHaveAccessibleName('Settings');
  },
);

it('gives up the Settings label only where the row is too narrow for it', async () => {
  getUpdateState().latestVersion = '2.0.0';
  host.style.width = `${SIDEBAR_MIN_WIDTH}px`;
  const view = render(SettingsFooter, { target: host, props: { onOpenSettings() {} } });
  await view.findByTestId('sidebar-update-button');
  const label = view.getByTestId('sidebar-settings-label');
  expect(label.getBoundingClientRect().width).toBe(0);

  host.style.width = '280px';
  expect(label.getBoundingClientRect().width).toBeGreaterThan(30);
});

it('keeps the Settings label on the narrowest sidebar when no update is waiting', () => {
  host.style.width = `${SIDEBAR_MIN_WIDTH}px`;
  const view = render(SettingsFooter, { target: host, props: { onOpenSettings() {} } });
  const settings = view.getByTestId('sidebar-settings-button');
  expect(view.getByTestId('sidebar-settings-label').getBoundingClientRect().width).toBeGreaterThan(30);
  expectRowFits(settings.parentElement!);
});
