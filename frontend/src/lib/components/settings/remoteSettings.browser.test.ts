import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import SettingsView from './SettingsView.svelte';
import SystemsSection from './SystemsSection.svelte';
import RemoteAccessSettings from './RemoteAccessSettings.svelte';
import { seedSettingsPages } from '../../../test/helpers/settingsPages';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { showSettingsRail } from '../../stores/settingsOverlay.svelte';
import { resetStagedBackends } from '../../../test/helpers/backends';

let host: HTMLDivElement;
beforeEach(async () => {
  setCompactLayoutForTest(true);
  await seedSettingsPages();
  host = document.createElement('div');
  host.style.cssText = 'width:360px;height:800px;background:var(--color-surface-0);color:var(--color-fg)';
  document.body.append(host);
});
afterEach(() => { cleanup(); host.remove(); resetStagedBackends(); setCompactLayoutForTest(false); });

it('exposes every remote page in mobile navigation and opens pairing directly', async () => {
  showSettingsRail();
  const view = render(SettingsView, { target: host, props: { onClose() {} } });
  for (const name of ['Connections', 'Pairing & network', 'Accounts', 'Agent access']) {
    expect(view.getByRole('tab', { name }).getBoundingClientRect().width).toBeGreaterThan(100);
  }
  await fireEvent.click(view.getByRole('tab', { name: 'Pairing & network' }));
  await waitFor(() => expect(view.getByRole('button', { name: 'Pair a device' })).toBeTruthy());
  expect(view.getByText('Advanced network settings').closest('details')?.open).toBe(false);
  expect(host.scrollWidth).toBeLessThanOrEqual(361);
});

it.each([320, 360, 412])('keeps connection setup and pairing inside %ipx', async (width) => {
  host.style.width = `${width}px`;
  host.style.padding = '16px';
  const connections = render(SystemsSection, { target: host });
  await waitFor(() => expect(connections.getByText('Connect another computer')).toBeTruthy());
  expect(host.scrollWidth).toBeLessThanOrEqual(width + 1);
  connections.unmount();
  const pairing = render(RemoteAccessSettings, { target: host });
  await waitFor(() => expect(pairing.getByText('Allow LAN connections')).toBeTruthy());
  expect(host.scrollWidth).toBeLessThanOrEqual(width + 1);
  await waitFor(() => expect(pairing.getByText('Security & passkeys').closest('details')?.open).toBe(false));
});
