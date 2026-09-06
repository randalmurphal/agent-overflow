import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import SettingsFooter from './SettingsFooter.svelte';

const mode = vi.hoisted(() => ({ frontend: false }));
vi.mock('../../transport/runMode', async (original) => ({
  ...await original<typeof import('../../transport/runMode')>(),
  isFrontendOnly: () => mode.frontend,
}));

// The app's one ambient read-only marker. It is a MODE indicator, so its
// predicate is the grant set (transport/scopes.ts isViewOnly) and never
// the device class — and the two states that must never show it are the
// owner's own screen and a full-access paired device.
describe('SettingsFooter view-only indicator', () => {
  beforeEach(() => {
    mode.frontend = false;
    resetBindingMocks();
    resetToLocalPage();
  });

  afterEach(() => {
    mode.frontend = false;
    resetToLocalPage();
    resetBindingMocks();
  });

  it('has no execution-host sleep control in a standalone frontend', () => {
    mode.frontend = true;
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.queryByTestId('sidebar-keep-awake-toggle')).toBeNull();
    expect(view.getByTestId('sidebar-settings-button')).toBeVisible();
  });

  it('shows for a session holding the observe set alone', async () => {
    await pairViewOnly();
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.getByTestId('view-only-indicator')).toHaveTextContent('View only');
  });

  it('stays hidden on the local page', () => {
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.queryByTestId('view-only-indicator')).toBeNull();
  });

  it('stays hidden for a full-access device', async () => {
    await pairWithScopes(['threads:read', 'files:read', 'settings:read', 'threads:operate']);
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.queryByTestId('view-only-indicator')).toBeNull();
  });

  it('appears without a remount when the grant set narrows mid-session', async () => {
    await pairWithScopes(['threads:read', 'git:operate']);
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.queryByTestId('view-only-indicator')).toBeNull();

    await pairViewOnly();
    expect(await view.findByTestId('view-only-indicator')).toBeTruthy();
  });
});
