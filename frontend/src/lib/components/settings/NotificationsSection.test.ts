import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import NotificationsSection from './NotificationsSection.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

// The built-in cues are assets this bundle plays itself; jsdom has no audio
// engine, so the player is stubbed and the assertion is which of the two
// preview paths a click took.
const playNotificationCue = vi.fn();
vi.mock('../../stores/notificationSound', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/notificationSound')>()),
  playNotificationCue: (cue: string) => playNotificationCue(cue),
}));

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
    playNotificationCue.mockClear();
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

  // Not a kind: the one narrowing of the kinds above, and the one row that
  // defaults OFF, because a thread the sidebar does not list is not worth an
  // interruption until the user says so.
  it('renders threads-not-in-the-sidebar off by default and dispatches it as its own key', async () => {
    const { getByRole } = render(NotificationsSection);
    const toggle = getByRole('switch', { name: 'Toggle notifications for threads not in the sidebar' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(toggle);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ notifyHiddenThreads: true });
  });

  it('hides the threads-not-in-the-sidebar row beneath the master switch too', async () => {
    await seed({ notificationsEnabled: false, notifyHiddenThreads: true });
    const { queryByRole } = render(NotificationsSection);
    expect(queryByRole('switch', { name: 'Toggle notifications for threads not in the sidebar' })).toBeNull();
  });

  it('reflects a single kind turned off without touching the others', async () => {
    await seed({ notifyTurnComplete: false });
    const { getByRole } = render(NotificationsSection);
    expect(getByRole('switch', { name: 'Toggle turn complete notifications' })
      .getAttribute('aria-checked')).toBe('false');
    expect(getByRole('switch', { name: 'Toggle approval needed notifications' })
      .getAttribute('aria-checked')).toBe('true');
  });

  // The third stack. Three EVENTS, not six kinds: the cue answers "finished",
  // "needs you" or "something is wrong", and any built-in cue may be
  // assigned to any of them.
  describe('sounds', () => {
    const soundEvents: Array<[string, keyof Settings, keyof Settings, string, string]> = [
      ['Toggle the Turn complete cue', 'notifySoundTurnComplete', 'notifySoundCueTurnComplete', 'turn-complete', 'swoosh'],
      ['Toggle the Approval needed cue', 'notifySoundInputNeeded', 'notifySoundCueInputNeeded', 'input-needed', 'knock'],
      ['Toggle the Attention cue', 'notifySoundAttention', 'notifySoundCueAttention', 'attention', 'hum'],
    ];
    const cueOptions = ['swoosh', 'marimba', 'chord', 'knock', 'pop', 'hum', 'boop', 'system'];

    it('ships with sounds on and every event on its own cue', () => {
      const { getByRole, getByTestId } = render(NotificationsSection);
      expect(getByRole('switch', { name: 'Toggle notification sounds' }).getAttribute('aria-checked'))
        .toBe('true');
      for (const [name, , , testid, cue] of soundEvents) {
        expect(getByRole('switch', { name }).getAttribute('aria-checked')).toBe('true');
        expect((getByTestId(`settings-sound-cue-${testid}`) as HTMLSelectElement).value).toBe(cue);
      }
    });

    it.each(soundEvents)('dispatches %s as its own key', async (name, key) => {
      const { getByRole } = render(NotificationsSection);
      await fireEvent.click(getByRole('switch', { name }));

      const mock = getBindingMock('UpdateSettings');
      expect(mock!.mock.calls[0][0]).toEqual({ [key]: false });
    });

    it.each(soundEvents)('dispatches the cue chosen for %s', async (_name, _key, cueKey, testid) => {
      const { getByTestId } = render(NotificationsSection);
      const select = getByTestId(`settings-sound-cue-${testid}`) as HTMLSelectElement;
      await fireEvent.change(select, { target: { value: 'boop' } });

      const mock = getBindingMock('UpdateSettings');
      expect(mock!.mock.calls[0][0]).toEqual({ [cueKey]: 'boop' });
    });

    it('offers every built-in cue, and the system sound, for every event', () => {
      const { getByTestId } = render(NotificationsSection);
      for (const [, , , testid] of soundEvents) {
        const options = Array.from(
          (getByTestId(`settings-sound-cue-${testid}`) as HTMLSelectElement).options,
        ).map((option) => option.value);
        expect(options).toEqual(cueOptions);
      }
    });

    it('hides the per-event rows when the master sound switch is off', async () => {
      await seed({ notificationSoundsEnabled: false });
      const { getByRole, queryByRole, queryByTestId } = render(NotificationsSection);
      expect(getByRole('switch', { name: 'Toggle notification sounds' }).getAttribute('aria-checked'))
        .toBe('false');
      for (const [name, , , testid] of soundEvents) {
        expect(queryByRole('switch', { name })).toBeNull();
        expect(queryByTestId(`settings-sound-cue-${testid}`)).toBeNull();
      }
    });

    it('disables the cue picker for an event that is off, leaving its choice visible', async () => {
      await seed({ notifySoundTurnComplete: false });
      const { getByTestId } = render(NotificationsSection);
      expect((getByTestId('settings-sound-cue-turn-complete') as HTMLSelectElement).disabled).toBe(true);
      expect((getByTestId('settings-sound-cue-input-needed') as HTMLSelectElement).disabled).toBe(false);
    });

    // A cue picker is unusable without a way to hear the choice, and the
    // click that plays one is also the gesture the engine needs before the
    // page may make any sound at all.
    it('offers a preview button per event', () => {
      const { getByTestId } = render(NotificationsSection);
      for (const [, , , testid] of soundEvents) {
        expect(getByTestId(`settings-sound-preview-${testid}`)).toBeTruthy();
      }
    });

    // Two sounds, two sources. A built-in lives in this bundle, so the page
    // plays it; the system sound is not a file this app owns and only ever
    // arrives attached to a banner, so previewing it means asking the host
    // to raise one.
    it('plays a built-in cue locally, without asking the host for a banner', async () => {
      const preview = setBindingMock('PreviewNotificationSound', async () => undefined);
      const { getByTestId } = render(NotificationsSection);
      await fireEvent.click(getByTestId('settings-sound-preview-turn-complete'));

      expect(playNotificationCue).toHaveBeenCalledWith('swoosh');
      expect(preview).not.toHaveBeenCalled();
    });

    it.each(soundEvents)('sends a test notification for %s when its cue is the system sound',
      async (_name, _key, cueKey, testid) => {
        await seed({ [cueKey]: 'system' } as Partial<Settings>);
        const preview = setBindingMock('PreviewNotificationSound', async () => undefined);
        const { getByTestId } = render(NotificationsSection);
        await fireEvent.click(getByTestId(`settings-sound-preview-${testid}`));

        expect(preview.mock.calls[0][0]).toBe(testid);
        expect(playNotificationCue).not.toHaveBeenCalled();
      });

    // The preview is a real OS notification, so it can be refused. A click
    // that makes no sound and says nothing reads as a broken speaker.
    it('surfaces a refused test notification beside the controls', async () => {
      await seed({ notifySoundCueTurnComplete: 'system' });
      setBindingMock('PreviewNotificationSound', async () => {
        throw new Error('OS notifications are unavailable in this application mode');
      });
      const { getByTestId, findByRole } = render(NotificationsSection);
      await fireEvent.click(getByTestId('settings-sound-preview-turn-complete'));

      const alert = await findByRole('alert');
      expect(alert.textContent).toContain('unavailable');
    });

    // The button stays live under the system cue: it is the only way to hear
    // that sound at all. Only an event whose sound is OFF has nothing to play.
    it('keeps the preview clickable under the system cue and disables it when the event is off', async () => {
      await seed({ notifySoundCueTurnComplete: 'system', notifySoundInputNeeded: false });
      const { getByTestId } = render(NotificationsSection);
      expect((getByTestId('settings-sound-preview-turn-complete') as HTMLButtonElement).disabled)
        .toBe(false);
      expect((getByTestId('settings-sound-preview-input-needed') as HTMLButtonElement).disabled)
        .toBe(true);
    });

    it('hides the whole sound stack beneath the notifications master switch', async () => {
      await seed({ notificationsEnabled: false });
      const { queryByRole } = render(NotificationsSection);
      expect(queryByRole('switch', { name: 'Toggle notification sounds' })).toBeNull();
    });
  });
});
