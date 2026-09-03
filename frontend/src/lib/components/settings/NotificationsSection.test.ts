import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import NotificationsSection from './NotificationsSection.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  // The notifications block below reads the push status on mount.
  setBindingMock('GetPushSenderStatus', async () => ({
    configured: false,
    projectId: '',
    clientEmail: '',
    lastError: '',
    registeredDevices: 0,
  }));
  await loadSettings();
  return merged;
}

const perKind: Array<[string, keyof Settings]> = [
  ['Toggle turn complete notifications', 'notifyTurnComplete'],
  ['Toggle approval needed notifications', 'notifyApprovalNeeded'],
  ['Toggle error notifications', 'notifyError'],
  ['Toggle provider signed out notifications', 'notifyProviderSignedOut'],
  ['Toggle workflow needs attention notifications', 'notifyWorkflowAttention'],
  ['Toggle app update notifications', 'notifyAppUpdate'],
];

// The second stack answers a different question, so it is a separate table:
// these are not per-kind toggles and only one of them defaults on.
const quietWhen: Array<[string, keyof Settings, boolean]> = [
  ['Toggle quiet when this window is focused', 'notifyMuteWhenFocused', true],
  ['Toggle quiet when the thread is on screen', 'notifyMuteWhenThreadVisible', false],
];

describe('<NotificationsSection>', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders every kind on, because notifications were unconditional before these keys', async () => {
    const { getByTestId, getByRole } = render(NotificationsSection);
    expect(getByTestId('settings-notifications-section')).toBeTruthy();
    for (const name of ['Toggle desktop notifications', ...perKind.map(([label]) => label)]) {
      expect(getByRole('switch', { name }).getAttribute('aria-checked')).toBe('true');
    }
  });

  it.each(perKind)('dispatches %s as its own key', async (name, key) => {
    const { getByRole } = render(NotificationsSection);
    await fireEvent.click(getByRole('switch', { name }));

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ [key]: false });
  });

  it('renders the quiet-when stack at its own defaults', async () => {
    const { getByRole } = render(NotificationsSection);
    for (const [name, , on] of quietWhen) {
      expect(getByRole('switch', { name }).getAttribute('aria-checked')).toBe(String(on));
    }
  });

  it.each(quietWhen)('dispatches %s as its own key', async (name, key, on) => {
    const { getByRole } = render(NotificationsSection);
    await fireEvent.click(getByRole('switch', { name }));

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ [key]: !on });
  });

  it('hides every row beneath the master switch when it is off', async () => {
    await seed({ notificationsEnabled: false });
    const { getByRole, queryByRole } = render(NotificationsSection);
    expect(getByRole('switch', { name: 'Toggle desktop notifications' }).getAttribute('aria-checked'))
      .toBe('false');
    for (const [name] of [...perKind, ...quietWhen]) {
      expect(queryByRole('switch', { name })).toBeNull();
    }
  });

  it('reflects a single kind turned off without touching the others', async () => {
    await seed({ notifyTurnComplete: false });
    const { getByRole } = render(NotificationsSection);
    expect(getByRole('switch', { name: 'Toggle turn complete notifications' })
      .getAttribute('aria-checked')).toBe('false');
    expect(getByRole('switch', { name: 'Toggle approval needed notifications' })
      .getAttribute('aria-checked')).toBe('true');
  });
});
