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
  constructor(readonly src: string) {
    FakeAudio.instances.push(this);
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
});

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
