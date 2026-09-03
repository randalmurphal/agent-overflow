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

// The second stack answers a different question and is one picker, not a
// table of toggles: its four readings are exclusive.
function quietWhenRadio(container: HTMLElement, value: string): HTMLInputElement {
  const input = container.querySelector<HTMLInputElement>(
    `[data-testid="quiet-when-option-${value}"] input[type="radio"]`,
  );
  if (!input) throw new Error(`no quiet-when option ${value}`);
  return input;
}

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

  it('renders the quiet-when picker at its default, quiet while this window is focused', async () => {
    const { container } = render(NotificationsSection);
    for (const value of ['never', 'focused', 'threadVisible', 'focusedAndThreadVisible']) {
      expect(quietWhenRadio(container, value).checked).toBe(value === 'focused');
    }
  });

  it('dispatches a quiet-when choice as the one picker key', async () => {
    const { container } = render(NotificationsSection);
    await fireEvent.click(quietWhenRadio(container, 'focusedAndThreadVisible'));

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ notifyQuietWhen: 'focusedAndThreadVisible' });
  });

  it('hides every row beneath the master switch when it is off', async () => {
    await seed({ notificationsEnabled: false });
    const { getByRole, queryByRole } = render(NotificationsSection);
    expect(getByRole('switch', { name: 'Toggle desktop notifications' }).getAttribute('aria-checked'))
      .toBe('false');
    for (const [name] of perKind) {
      expect(queryByRole('switch', { name })).toBeNull();
    }
    expect(queryByRole('radiogroup', { name: 'Quiet when' })).toBeNull();
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
