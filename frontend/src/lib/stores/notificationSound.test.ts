import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  NOTIFICATION_SOUND_COOLDOWN_MS,
  applyNotificationSoundEvent,
  installNotificationSoundUnlock,
  playNotificationCue,
  __resetNotificationSoundForTest,
  __unlockNotificationSoundForTest,
} from './notificationSound';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import {
  __resetCustomSoundsForTest,
  customSoundUrl,
  ensureCustomSounds,
} from './sounds.svelte';

vi.mock('../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

const reported = vi.mocked(reportFrontendDiagnostic);

/** A speaker whose every call is observable and whose play() is scriptable. */
class FakeAudio {
  static instances: FakeAudio[] = [];
  static behavior: 'resolve' | 'reject' | 'throw' | 'undefined' = 'resolve';
  currentTime = 7;
  preload = 'none';
  plays = 0;
  pauses = 0;
  loads = 0;
  src: string;
  constructor(src: string) {
    this.src = src;
    FakeAudio.instances.push(this);
  }
  // The three calls that unhook an element from a revoked object URL. An
  // element that kept one would hold the decoded audio and play nothing.
  pause(): void {
    this.pauses += 1;
  }
  removeAttribute(name: string): void {
    if (name === 'src') this.src = '';
  }
  load(): void {
    this.loads += 1;
  }
  play(): Promise<void> | undefined {
    this.plays += 1;
    switch (FakeAudio.behavior) {
      case 'reject':
        return Promise.reject(new Error('NotAllowedError'));
      case 'throw':
        throw new Error('no audio device');
      case 'undefined':
        return undefined;
      default:
        return Promise.resolve();
    }
  }
}

function totalPlays(): number {
  return FakeAudio.instances.reduce((sum, audio) => sum + audio.plays, 0);
}

let clock = 0;

beforeEach(() => {
  __resetNotificationSoundForTest();
  reported.mockClear();
  FakeAudio.instances = [];
  FakeAudio.behavior = 'resolve';
  clock = 1_000;
  vi.stubGlobal('Audio', FakeAudio);
  vi.spyOn(performance, 'now').mockImplementation(() => clock);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  __resetNotificationSoundForTest();
  __resetCustomSoundsForTest();
  resetBindingMocks();
});

/** base64 of one byte, enough to become a Blob and an object URL. */
const CUE_BYTES = btoa('\x00');

/** Seed the backend's cue library and wait for this screen to hold it. */
async function seedLibrary(ids: string[]): Promise<void> {
  setBindingMock('GetSoundFiles', () => ({
    dir: '/cfg/sounds',
    sounds: ids.map((id) => ({ id, wav: CUE_BYTES })),
    warnings: [],
  }));
  ensureCustomSounds();
  await vi.waitFor(() => {
    for (const id of ids) expect(customSoundUrl(id)).not.toBeNull();
  });
}

describe('notification cue playback', () => {
  it('plays a built-in cue once the page has been interacted with', () => {
    __unlockNotificationSoundForTest();
    playNotificationCue('swoosh');

    expect(FakeAudio.instances).toHaveLength(1);
    expect(FakeAudio.instances[0].plays).toBe(1);
    // Restarted from the top so a cue cut short by the cooldown does not
    // resume mid-tone the next time.
    expect(FakeAudio.instances[0].currentTime).toBe(0);
    expect(reported).not.toHaveBeenCalled();
  });

  it('reuses one element per cue rather than allocating per notification', () => {
    __unlockNotificationSoundForTest();
    playNotificationCue('swoosh');
    clock += NOTIFICATION_SOUND_COOLDOWN_MS;
    playNotificationCue('swoosh');

    expect(FakeAudio.instances).toHaveLength(1);
    expect(FakeAudio.instances[0].plays).toBe(2);
  });

  it('collapses a burst into a single sound', () => {
    __unlockNotificationSoundForTest();
    for (let i = 0; i < 4; i += 1) playNotificationCue('swoosh');
    expect(totalPlays()).toBe(1);

    // Still inside the window, even for a different cue: the cooldown is a
    // property of the speaker, not of one sound.
    clock += NOTIFICATION_SOUND_COOLDOWN_MS - 1;
    playNotificationCue('hum');
    expect(totalPlays()).toBe(1);

    clock += 1;
    playNotificationCue('hum');
    expect(totalPlays()).toBe(2);
  });

  it('stays silent, with a note, before the first user gesture', () => {
    playNotificationCue('swoosh');

    expect(FakeAudio.instances).toHaveLength(0);
    expect(reported).toHaveBeenCalledWith(
      'notification cue suppressed before first user gesture',
      'swoosh',
    );
  });

  it('arms playback on the first pointer or key event', () => {
    const teardown = installNotificationSoundUnlock();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'a' }));

    playNotificationCue('swoosh');
    expect(totalPlays()).toBe(1);
    teardown();
  });

  it('drops its listeners on teardown', () => {
    const teardown = installNotificationSoundUnlock();
    teardown();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'a' }));

    playNotificationCue('swoosh');
    expect(totalPlays()).toBe(0);
  });

  it('reports an unknown cue instead of substituting another sound', () => {
    __unlockNotificationSoundForTest();
    playNotificationCue('fanfare');

    expect(FakeAudio.instances).toHaveLength(0);
    expect(reported).toHaveBeenCalledWith('notification cue is not a built-in', 'fanfare');
  });

  it('reports a rejected play without throwing', async () => {
    __unlockNotificationSoundForTest();
    FakeAudio.behavior = 'reject';
    expect(() => playNotificationCue('swoosh')).not.toThrow();

    await vi.waitFor(() => {
      expect(reported).toHaveBeenCalledWith(
        'notification cue playback failed',
        expect.stringContaining('swoosh'),
      );
    });
  });

  it('reports a throwing play without throwing', () => {
    __unlockNotificationSoundForTest();
    FakeAudio.behavior = 'throw';
    expect(() => playNotificationCue('swoosh')).not.toThrow();

    expect(reported).toHaveBeenCalledWith(
      'notification cue playback failed',
      expect.stringContaining('no audio device'),
    );
  });

  it('tolerates an engine whose play() returns no promise', () => {
    __unlockNotificationSoundForTest();
    FakeAudio.behavior = 'undefined';
    expect(() => playNotificationCue('swoosh')).not.toThrow();
    expect(reported).not.toHaveBeenCalled();
  });

  it('fetches nothing for a screen that never raises a notification', () => {
    expect(FakeAudio.instances).toHaveLength(0);
  });
});

describe('notification:sound frames', () => {
  beforeEach(() => {
    __unlockNotificationSoundForTest();
  });

  it('plays the cue the host named', () => {
    applyNotificationSoundEvent({ event: 'input-needed', cue: 'knock' });
    expect(totalPlays()).toBe(1);
    expect(FakeAudio.instances[0].src).toContain('knock');
  });

  it('plays the cue rather than re-deriving one from the event', () => {
    // The host resolved this screen's preference to a different cue; the
    // player does not second-guess it.
    applyNotificationSoundEvent({ event: 'turn-complete', cue: 'hum' });
    expect(FakeAudio.instances[0].src).toContain('hum');
  });

  it('ignores a malformed frame', () => {
    for (const frame of [null, undefined, 'turn-complete', [], {}, { cue: '' }, { cue: 4 }]) {
      applyNotificationSoundEvent(frame);
    }
    expect(totalPlays()).toBe(0);
    expect(reported).not.toHaveBeenCalled();
  });
});

// A `custom:<id>` names a file in the BACKEND HOST's sounds directory, so the
// bytes come from the library this screen holds rather than from the bundle.
describe('custom cues', () => {
  beforeEach(() => {
    __unlockNotificationSoundForTest();
  });

  it('plays a custom cue from the library the backend listed', async () => {
    await seedLibrary(['desk-bell']);

    applyNotificationSoundEvent({ event: 'turn-complete', cue: 'custom:desk-bell' });

    expect(totalPlays()).toBe(1);
    expect(FakeAudio.instances[0].src).toBe(customSoundUrl('desk-bell'));
    expect(reported).not.toHaveBeenCalled();
  });

  // The frame can name a cue this screen does not have: deleted a second ago,
  // or listed on a host whose reply has not landed. A notification that makes
  // no sound at all is worse than one that makes the usual sound.
  it("substitutes the event's default for a cue this screen does not have", async () => {
    await seedLibrary([]);

    applyNotificationSoundEvent({ event: 'turn-complete', cue: 'custom:desk-bell' });

    expect(totalPlays()).toBe(1);
    // SETTINGS_DEFAULTS.notifySoundCueTurnComplete, from the generated mirror
    // of DefaultSettings — not a reading of what the user chose.
    expect(FakeAudio.instances[0].src).toContain('swoosh');
    expect(reported).toHaveBeenCalledWith(
      'custom notification cue is missing, played the default',
      'custom:desk-bell',
    );
  });

  it.each([
    ['input-needed', 'knock'],
    ['attention', 'hum'],
  ])('substitutes the %s default', async (event, fallback) => {
    await seedLibrary([]);

    applyNotificationSoundEvent({ event, cue: 'custom:gone' });

    expect(FakeAudio.instances[0].src).toContain(fallback);
  });

  // With no event there is no defined substitution, so the honest outcome is
  // silence and a note, exactly as for a cue value nobody recognises.
  it('stays silent for a missing custom cue with no event to fall back on', async () => {
    await seedLibrary([]);

    playNotificationCue('custom:desk-bell');

    expect(FakeAudio.instances).toHaveLength(0);
    expect(reported).toHaveBeenCalledWith(
      'notification cue is not a built-in',
      'custom:desk-bell',
    );
  });

  // The object URL behind a cached element is revoked the moment the listing
  // is replaced. Keeping the element would mean a cue that silently plays
  // nothing for the rest of the page's life.
  it('drops its cached custom elements when the library changes', async () => {
    await seedLibrary(['desk-bell']);
    const teardown = installNotificationSoundUnlock();
    playNotificationCue('custom:desk-bell');
    const stale = FakeAudio.instances[0];
    expect(stale.src).not.toBe('');

    setBindingMock('GetSoundFiles', () => ({
      dir: '/cfg/sounds',
      sounds: [{ id: 'desk-bell', wav: CUE_BYTES }],
      warnings: [],
    }));
    emitWailsEvent('sound:changed', null);
    await vi.waitFor(() => {
      expect(stale.src).toBe('');
    });
    expect(stale.pauses).toBe(1);
    expect(stale.loads).toBe(1);

    // The next play mints a fresh element over the URL that is live now.
    clock += NOTIFICATION_SOUND_COOLDOWN_MS;
    playNotificationCue('custom:desk-bell');
    expect(FakeAudio.instances).toHaveLength(2);
    expect(FakeAudio.instances[1].src).toBe(customSoundUrl('desk-bell'));
    teardown();
  });

  // Built-in elements are backed by bundle URLs that nothing revokes, so a
  // library change must not throw them away.
  it('keeps built-in elements across a library change', async () => {
    await seedLibrary(['desk-bell']);
    const teardown = installNotificationSoundUnlock();
    playNotificationCue('swoosh');
    const builtin = FakeAudio.instances[0];

    emitWailsEvent('sound:changed', null);
    await vi.waitFor(() => {
      expect(customSoundUrl('desk-bell')).not.toBeNull();
    });
    clock += NOTIFICATION_SOUND_COOLDOWN_MS;
    playNotificationCue('swoosh');

    expect(FakeAudio.instances).toHaveLength(1);
    expect(builtin.plays).toBe(2);
    teardown();
  });

  // `CUE_URLS['constructor']` is a truthy INHERITED property. A cue value
  // arrives on the wire, so the lookup has to be an own-property check.
  it('refuses a cue value that names an inherited property', () => {
    playNotificationCue('constructor');

    expect(FakeAudio.instances).toHaveLength(0);
    expect(reported).toHaveBeenCalledWith('notification cue is not a built-in', 'constructor');
  });
});
