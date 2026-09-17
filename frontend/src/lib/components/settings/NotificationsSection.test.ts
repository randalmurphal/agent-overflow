import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import NotificationsSection from './NotificationsSection.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { __resetCustomSoundsForTest } from '../../stores/sounds.svelte';
import { renderCueWav } from '../../audio/renderCue';

// The built-in cues are assets this bundle plays itself; happy-dom has no
// audio engine, so the player is stubbed and the assertion is which of the
// two preview paths a click took, and with which cue.
const playNotificationCue = vi.fn();
vi.mock('../../stores/notificationSound', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/notificationSound')>()),
  playNotificationCue: (cue: string, event?: string) => playNotificationCue(cue, event),
}));

// happy-dom has no Web Audio either, so the decode-and-re-render step is
// stubbed here. Its own contract — that what it emits is the canonical WAV
// internal/soundlib accepts — is pinned by lib/audio/renderCue.test.ts.
vi.mock('../../audio/renderCue', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../audio/renderCue')>()),
  renderCueWav: vi.fn(),
}));

const renderCue = vi.mocked(renderCueWav);

/** The wire body PutSoundFile should carry for `bytes`. */
function base64Of(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes));
}

/** Seed the backend's cue library for the render that follows. */
function seedSounds(ids: string[], warnings: string[] = [], dir = '/cfg/sounds'): void {
  setBindingMock('GetSoundFiles', () => ({
    dir,
    sounds: ids.map((id) => ({ id, wav: btoa('\x00') })),
    warnings,
  }));
}

/** Hand the hidden input a file and fire the change the component listens for. */
async function pickFile(input: HTMLInputElement, file: File): Promise<void> {
  Object.defineProperty(input, 'files', { value: [file], configurable: true });
  await fireEvent.change(input);
}

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
  // The sound stack reads the backend's cue library on mount. Tests that
  // care about its contents re-seed it before rendering.
  seedSounds([]);
  await loadSettings();
  return merged;
}

/** Render, then let the library listing land before asserting on the pickers. */
async function renderSection() {
  const view = render(NotificationsSection);
  await vi.waitFor(() => {
    expect(getBindingMock('GetSoundFiles')).toHaveBeenCalled();
  });
  await Promise.resolve();
  await Promise.resolve();
  return view;
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
    renderCue.mockReset();
    await seed();
  });

  afterEach(() => {
    __resetCustomSoundsForTest();
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

      // The event travels with the cue so a missing custom cue falls back to
      // that event's default, exactly as a real notification would.
      expect(playNotificationCue).toHaveBeenCalledWith('swoosh', 'turn-complete');
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

  // The library belongs to the BACKEND, not to this screen: every screen
  // attached to that computer offers the same cues, and Go re-validates every
  // file on every listing.
  describe('custom sounds', () => {
    it('offers the backend library after the built-ins, in one Custom group', async () => {
      seedSounds(['desk-bell', 'zap']);
      const { getByTestId } = await renderSection();

      const select = getByTestId('settings-sound-cue-turn-complete') as HTMLSelectElement;
      const group = select.querySelector('optgroup');
      expect(group?.getAttribute('label')).toBe('Custom');
      expect(Array.from(select.options).map((option) => option.value)).toEqual([
        'swoosh', 'marimba', 'chord', 'knock', 'pop', 'hum', 'boop', 'system',
        'custom:desk-bell', 'custom:zap',
      ]);
    });

    it('dispatches a custom choice as the same cue key', async () => {
      seedSounds(['desk-bell']);
      const { getByTestId } = await renderSection();
      await fireEvent.change(getByTestId('settings-sound-cue-attention'), {
        target: { value: 'custom:desk-bell' },
      });

      const mock = getBindingMock('UpdateSettings');
      expect(mock!.mock.calls[0][0]).toEqual({ notifySoundCueAttention: 'custom:desk-bell' });
    });

    it('lists each cue with a way to hear it and a way to remove it', async () => {
      seedSounds(['desk-bell']);
      const { getByTestId } = await renderSection();

      await fireEvent.click(getByTestId('settings-sound-play-desk-bell'));
      expect(playNotificationCue).toHaveBeenCalledWith('custom:desk-bell', undefined);

      const remove = setBindingMock('DeleteSoundFile', async () => undefined);
      await fireEvent.click(getByTestId('settings-sound-delete-desk-bell'));
      await vi.waitFor(() => {
        expect(remove).toHaveBeenCalledWith('desk-bell');
      });
    });

    // A failed delete must reach the user: a row that stays put with no
    // explanation reads as a broken button.
    it('shows a refused delete inline', async () => {
      seedSounds(['desk-bell']);
      setBindingMock('DeleteSoundFile', async () => {
        throw new Error('sounds directory is read-only');
      });
      const { getByTestId, findByRole } = await renderSection();
      await fireEvent.click(getByTestId('settings-sound-delete-desk-bell'));

      expect((await findByRole('alert')).textContent).toContain('read-only');
    });

    // The file is decoded and re-rendered in this page's own engine; what
    // reaches the host is a canonical WAV this page wrote.
    it('renders a picked file and sends the bytes under an id from its name', async () => {
      const wav = new Uint8Array([82, 73, 70, 70, 1, 2, 3, 4]);
      renderCue.mockResolvedValue({ wav, seconds: 0.4 });
      const put = setBindingMock('PutSoundFile', async () => undefined);
      const { getByTestId } = await renderSection();

      await pickFile(
        getByTestId('settings-sound-file') as HTMLInputElement,
        new File([new Uint8Array(4)], 'Desk Bell (Final).mp3', { type: 'audio/mpeg' }),
      );

      await vi.waitFor(() => {
        expect(put).toHaveBeenCalled();
      });
      expect(put.mock.calls[0]).toEqual(['desk-bell-final', base64Of(wav)]);
    });

    it.each([
      ['Desk Bell.wav', 'desk-bell'],
      ['  spaced  out .aiff', 'spaced-out'],
      ['Café Chime.mp3', 'cafe-chime'],
      ['♪♪♪.wav', 'sound'],
      ['UPPER_snake_case.ogg', 'upper-snake-case'],
    ])('derives the id %s -> %s', async (name, id) => {
      renderCue.mockResolvedValue({ wav: new Uint8Array([1]), seconds: 0.1 });
      const put = setBindingMock('PutSoundFile', async () => undefined);
      const { getByTestId } = await renderSection();

      await pickFile(getByTestId('settings-sound-file') as HTMLInputElement, new File([], name));

      await vi.waitFor(() => {
        expect(put).toHaveBeenCalled();
      });
      expect(put.mock.calls[0][0]).toBe(id);
    });

    // Every refusal the add can hit — a file the engine cannot decode, one
    // past the cap, a name already taken — is shown beside the button.
    it.each([
      ['a file the engine refuses', () => renderCue.mockRejectedValue(new Error('That file could not be decoded as audio (bad).')), 'could not be decoded'],
      ['a file past the cap', () => renderCue.mockRejectedValue(new Error('Keep it under 3 seconds')), 'under 3 seconds'],
    ])('shows %s inline', async (_name, arrange, message) => {
      arrange();
      setBindingMock('PutSoundFile', async () => undefined);
      const { getByTestId, findByRole } = await renderSection();

      await pickFile(getByTestId('settings-sound-file') as HTMLInputElement, new File([], 'x.mp3'));

      expect((await findByRole('alert')).textContent).toContain(message);
    });

    it('shows a host that refuses the write inline', async () => {
      renderCue.mockResolvedValue({ wav: new Uint8Array([1]), seconds: 0.1 });
      setBindingMock('PutSoundFile', async () => {
        throw new Error('a sound named "desk-bell" already exists; delete it first');
      });
      const { getByTestId, findByRole } = await renderSection();

      await pickFile(getByTestId('settings-sound-file') as HTMLInputElement, new File([], 'desk-bell.mp3'));

      expect((await findByRole('alert')).textContent).toContain('already exists');
    });

    // A file dropped in by hand is the expected way this directory grows, so
    // one the host cannot use has to explain itself here.
    it('shows the directory and the listing warnings', async () => {
      seedSounds([], ['bogus.wav: skipped, not a canonical cue'], '/cfg/sounds');
      const { getByTestId, getByText } = await renderSection();

      expect(getByTestId('settings-sound-warnings').textContent).toContain('bogus.wav');
      expect(getByText('/cfg/sounds')).toBeTruthy();
    });

    // The chosen cue is kept visible: a <select> whose value matches no
    // option renders blank, which would hide what the warning is about.
    it('keeps a cue the library has lost visible, with a warning under it', async () => {
      await seed({ notifySoundCueAttention: 'custom:gone' });
      seedSounds(['desk-bell']);
      const { getByTestId } = await renderSection();

      const select = getByTestId('settings-sound-cue-attention') as HTMLSelectElement;
      expect(select.value).toBe('custom:gone');
      expect(Array.from(select.options).map((option) => option.textContent?.trim())).toContain(
        'gone (missing)',
      );
      expect(getByTestId('settings-sound-missing-attention').textContent).toContain(
        'gone is missing; the default sound plays instead.',
      );
    });

    it('says nothing about a cue the library does hold', async () => {
      await seed({ notifySoundCueAttention: 'custom:desk-bell' });
      seedSounds(['desk-bell']);
      const { getByTestId, queryByTestId } = await renderSection();

      expect((getByTestId('settings-sound-cue-attention') as HTMLSelectElement).value)
        .toBe('custom:desk-bell');
      expect(queryByTestId('settings-sound-missing-attention')).toBeNull();
    });

    // Before the first listing arrives, "this backend has no cue by that
    // name" and "this screen has not asked yet" look identical. Only the
    // first is worth warning about.
    it('does not claim a cue is missing before the library has loaded', async () => {
      await seed({ notifySoundCueAttention: 'custom:desk-bell' });
      setBindingMock('GetSoundFiles', () => new Promise(() => {}));
      const { queryByTestId } = render(NotificationsSection);
      await Promise.resolve();

      expect(queryByTestId('settings-sound-missing-attention')).toBeNull();
    });

    it('hides the library with the rest of the stack when sounds are off', async () => {
      await seed({ notificationSoundsEnabled: false });
      seedSounds(['desk-bell']);
      const { queryByTestId } = render(NotificationsSection);
      await Promise.resolve();

      expect(queryByTestId('settings-sound-add')).toBeNull();
      expect(queryByTestId('settings-sound-play-desk-bell')).toBeNull();
    });
  });
});
