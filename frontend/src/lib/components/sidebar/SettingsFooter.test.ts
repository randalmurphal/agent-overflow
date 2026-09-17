import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetSettingsForTest, updateSetting } from '../../stores/settings.svelte';
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

// The quick mute. `notificationSoundsEnabled` is a DEVICE-tier key: it decides
// whether THIS screen plays a cue, not whether the machine notifies at all —
// so unlike the keep-awake toggle beside it the control carries no host gate
// and has to render for a standalone frontend and a view-only device too.
describe('SettingsFooter notification-sound toggle', () => {
  beforeEach(() => {
    mode.frontend = false;
    resetBindingMocks();
    resetSettingsForTest();
    resetToLocalPage();
  });

  afterEach(() => {
    mode.frontend = false;
    resetToLocalPage();
    resetSettingsForTest();
    resetBindingMocks();
  });

  it('renders unmuted by default, offering the mute', () => {
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    const button = view.getByTestId('sidebar-sound-toggle');
    expect(button).toHaveAttribute('aria-label', 'Mute notification sounds');
    expect(button).toHaveAttribute('aria-pressed', 'false');
    expect(button).toHaveAttribute(
      'title',
      'Notification sounds on for this screen. Click to mute.',
    );
  });

  it('writes the one device-tier key through the settings binding', async () => {
    setBindingMock('UpdateSettings', async () => undefined);
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    await fireEvent.click(view.getByTestId('sidebar-sound-toggle'));

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ notificationSoundsEnabled: false });
  });

  // The muted reading is the non-default one, so it is what the icon, the
  // pressed state and the label all have to announce.
  it('reads as muted once the setting is off, and offers the unmute', async () => {
    await updateSetting('notificationSoundsEnabled', false);
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    const button = view.getByTestId('sidebar-sound-toggle');
    expect(button).toHaveAttribute('aria-label', 'Unmute notification sounds');
    expect(button).toHaveAttribute('aria-pressed', 'true');
    expect(button).toHaveAttribute(
      'title',
      'Notification sounds muted on this screen. Click to unmute.',
    );
  });

  it('reverses the setting from the muted state', async () => {
    const mock = setBindingMock('UpdateSettings', async () => undefined);
    await updateSetting('notificationSoundsEnabled', false);
    mock.mockClear();

    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    await fireEvent.click(view.getByTestId('sidebar-sound-toggle'));
    expect(mock.mock.calls.at(-1)?.[0]).toEqual({ notificationSoundsEnabled: true });
  });

  it('renders in a standalone frontend, where the host-tier keep-awake does not', () => {
    mode.frontend = true;
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.queryByTestId('sidebar-keep-awake-toggle')).toBeNull();
    expect(view.getByTestId('sidebar-sound-toggle')).toBeVisible();
  });

  it('renders for a view-only session', async () => {
    await pairViewOnly();
    const view = render(SettingsFooter, { onOpenSettings: () => {} });
    expect(view.getByTestId('view-only-indicator')).toBeTruthy();
    expect(view.getByTestId('sidebar-sound-toggle')).toBeVisible();
  });
});
