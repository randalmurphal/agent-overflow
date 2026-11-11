// Moving through a reading by section, on both routes: where the reading
// is and which blocks each section covers, pause and resume through a voice
// that holds its place and one that can only stop, previous, next and
// "read from here" from playing, waiting and paused, back, and a held
// paused playback being stopped by everything that ends a reading.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { fakeClipEngines, fakeSpeechEngine } from '../../test/helpers/speech';
import { HOME_BACKEND } from '../transport/backendKey';
import {
  READ_ALOUD_BACK_SECONDS,
  pauseReadAloud,
  readAloud,
  readAloudBack,
  readAloudBackStep,
  readAloudFromSection,
  readAloudNextSection,
  readAloudPhase,
  readAloudPosition,
  readAloudPreviousSection,
  readAloudSectionBlocks,
  readAloudWaiting,
  resetReadAloudForTest,
  restartReadAloud,
  resumeReadAloud,
  stopReadAloud,
  useOpenAIEngineForTest,
  watchReadAloudVoices,
} from './readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from './readAloudPreferences.svelte';
import { refreshReadAloudSource, resetReadAloudSourcesForTest } from './readAloudSource.svelte';

// Three sections: blocks 0-1, block 3 (after a code block), blocks 4-5.
const REPLY = [
  '## Setup', '', 'Install it. Then run it.', '', '```sh', 'make', '```', '',
  'The build is done now and it passed every check that was asked of it, so it is ready to ship today.', '',
  '## Next', '', 'Ship it.',
].join('\n');
const BLOCKS = [{ first: 0, last: 1 }, { first: 3, last: 3 }, { first: 4, last: 5 }];
// The device reads sentences: section 0 is 0-2, section 1 is 3, section 2 is 4-5.
const DEVICE_SECTION_START = [0, 3, 4];
const OWNER = { computer: HOME_BACKEND };

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

describe('read-aloud by section', () => {
  let device: ReturnType<typeof fakeSpeechEngine>;
  let clips: ReturnType<typeof fakeClipEngines>;
  let available = true;

  beforeEach(async () => {
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    setReadAloudPreference('enabled', true);
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    device = fakeSpeechEngine();
    watchReadAloudVoices(device.engine);
    clips = fakeClipEngines();
    useOpenAIEngineForTest(clips.factory);
    available = true;
    setBindingMock('ReadAloudSource', async () => ({ available, voices: [{ id: 'marin', name: 'Marin' }] }));
    await refreshReadAloudSource(HOME_BACKEND);
    await settle();
  });

  afterEach(() => {
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    vi.restoreAllMocks();
  });

  const readOpenAI = (key = 'a'): void => readAloud(key, REPLY, OWNER, 'openai:marin');
  const readDevice = (key = 'a'): void => readAloud(key, REPLY, OWNER, 'v0');

  describe('position and blocks', () => {
    it('reads an OpenAI voice one section per piece and reports the section sounding', () => {
      expect(readAloudPosition('a')).toBeNull();
      expect(readAloudSectionBlocks('a')).toEqual([]);
      readOpenAI();
      expect(clips.playbacks[0].pieces).toHaveLength(3);
      expect(clips.playbacks[0].pieces[0]).toBe('Setup\n\nInstall it. Then run it.');
      expect(readAloudPosition('a')).toEqual({ section: 0, sections: 3 });
      expect(readAloudSectionBlocks('a')).toEqual(BLOCKS);
      expect(readAloudPosition('b')).toBeNull();
      expect(readAloudSectionBlocks('b')).toEqual([]);
      clips.playbacks[0].onPiece(2);
      expect(readAloudPosition('a')).toEqual({ section: 2, sections: 3 });
    });

    it('reads a device voice by sentence and maps each sentence to its section', () => {
      readDevice();
      expect(device.playbacks[0].pieces).toEqual([
        'Setup', 'Install it.', 'Then run it.',
        'The build is done now and it passed every check that was asked of it, so it is ready to ship today.',
        'Next', 'Ship it.',
      ]);
      expect(readAloudSectionBlocks('a')).toEqual(BLOCKS);
      for (const [sentence, section] of [[0, 0], [2, 0], [3, 1], [4, 2], [5, 2]]) {
        device.playbacks[0].onPiece(sentence);
        expect(readAloudPosition('a')).toEqual({ section, sections: 3 });
      }
    });

    it('keeps the position while paused and clears it when the reading ends', async () => {
      readDevice();
      device.playbacks[0].onPiece(3);
      pauseReadAloud();
      await settle();
      expect(readAloudPosition('a')).toEqual({ section: 1, sections: 3 });
      expect(readAloudSectionBlocks('a')).toEqual(BLOCKS);
      stopReadAloud();
      expect(readAloudPosition('a')).toBeNull();
      expect(readAloudSectionBlocks('a')).toEqual([]);

      readOpenAI();
      clips.playbacks[0].finish('ended');
      await settle();
      expect(readAloudPhase('a')).toBe('idle');
      expect(readAloudPosition('a')).toBeNull();
    });
  });

  describe('pause and resume', () => {
    it('through a voice that holds its place: no new playback, pause and resume once each', async () => {
      readOpenAI();
      clips.playbacks[0].onPiece(1);
      pauseReadAloud();
      pauseReadAloud();
      await settle();
      expect(readAloudPhase('a')).toBe('paused');
      expect(clips.playbacks[0].stopped).toBe(false);
      resumeReadAloud();
      resumeReadAloud();
      expect(readAloudPhase('a')).toBe('playing');
      expect(clips.playbacks).toHaveLength(1);
      expect(clips.playbacks[0].calls).toEqual(['pause', 'resume']);
      expect(readAloudPosition('a')).toEqual({ section: 1, sections: 3 });
    });

    it('through a device voice: stops, then speaks again from the sentence', async () => {
      readDevice();
      device.playbacks[0].onPiece(2);
      pauseReadAloud();
      pauseReadAloud();
      await settle();
      expect(device.playbacks[0].stopped).toBe(true);
      resumeReadAloud();
      resumeReadAloud();
      expect(device.playbacks).toHaveLength(2);
      expect(device.playbacks[1].from).toBe(2);
    });

    it('restart reads from the first section on both routes', async () => {
      readOpenAI();
      clips.playbacks[0].onPiece(2);
      pauseReadAloud();
      restartReadAloud();
      expect(clips.playbacks[0].calls).toEqual(['pause', 'skip 0']);
      expect(readAloudPhase('a')).toBe('playing');
      expect(readAloudPosition('a')).toEqual({ section: 0, sections: 3 });

      readDevice();
      device.playbacks[0].onPiece(4);
      pauseReadAloud();
      await settle();
      restartReadAloud();
      expect(device.playbacks[1].from).toBe(0);
    });
  });

  describe('jumps', () => {
    it('from playing, waiting and paused through OpenAI skip within the one playback and wait for the section', async () => {
      readOpenAI();
      // Waiting for the first section.
      expect(readAloudWaiting('a')).toBe(true);
      readAloudFromSection(2);
      expect(readAloudPosition('a')).toEqual({ section: 2, sections: 3 });
      expect(readAloudWaiting('a')).toBe(true);
      clips.playbacks[0].onPiece(2);
      expect(readAloudWaiting('a')).toBe(false);

      // Playing.
      readAloudPreviousSection();
      expect(readAloudPosition('a')).toEqual({ section: 1, sections: 3 });
      expect(readAloudWaiting('a')).toBe(true);
      clips.playbacks[0].onPiece(1);

      // Paused: the jump plays.
      pauseReadAloud();
      readAloudNextSection();
      expect(readAloudPhase('a')).toBe('playing');
      expect(clips.playbacks).toHaveLength(1);
      expect(clips.playbacks[0].calls).toEqual(['skip 2', 'skip 1', 'pause', 'skip 2']);

      // Twice in a row: still the one playback.
      readAloudFromSection(0);
      readAloudFromSection(0);
      expect(clips.playbacks).toHaveLength(1);
      expect(clips.playbacks[0].calls.slice(-2)).toEqual(['skip 0', 'skip 0']);
    });

    it('through a device voice speak again from the section’s first sentence, from playing and paused', async () => {
      readDevice();
      readAloudFromSection(2);
      expect(device.playbacks[0].stopped).toBe(true);
      expect(device.playbacks[1].from).toBe(DEVICE_SECTION_START[2]);
      expect(readAloudPosition('a')).toEqual({ section: 2, sections: 3 });

      readAloudPreviousSection();
      expect(device.playbacks[2].from).toBe(DEVICE_SECTION_START[1]);

      pauseReadAloud();
      await settle();
      readAloudNextSection();
      expect(readAloudPhase('a')).toBe('playing');
      expect(device.playbacks[3].from).toBe(DEVICE_SECTION_START[2]);
      expect(device.playbacks.slice(0, 3).every((p) => p.stopped)).toBe(true);
      // A device reading never waits.
      expect(readAloudWaiting('a')).toBe(false);
    });

    it('through an OpenAI voice that cannot skip, starts a new playback from the section and waits', () => {
      clips = fakeClipEngines({ holds: false });
      useOpenAIEngineForTest(clips.factory);
      readOpenAI();
      clips.playbacks[0].onPiece(0);
      readAloudNextSection();
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(clips.playbacks[1].from).toBe(1);
      expect(readAloudWaiting('a')).toBe(true);
    });

    it('previous in the first section goes to its start; next in the last does nothing', async () => {
      readOpenAI();
      clips.playbacks[0].onPiece(0);
      readAloudPreviousSection();
      expect(clips.playbacks[0].calls).toEqual(['skip 0']);
      readAloudFromSection(2);
      clips.playbacks[0].onPiece(2);
      readAloudNextSection();
      expect(clips.playbacks[0].calls).toEqual(['skip 0', 'skip 2']);
      expect(readAloudPhase('a')).toBe('playing');

      readDevice();
      device.playbacks[0].onPiece(1);
      readAloudPreviousSection();
      expect(device.playbacks[1].from).toBe(0);
      device.playbacks[1].onPiece(5);
      readAloudNextSection();
      expect(device.playbacks).toHaveLength(2);
    });

    it('does nothing out of range or with nothing read', () => {
      readAloudFromSection(0);
      readAloudPreviousSection();
      readAloudNextSection();
      readAloudBack();
      expect(clips.playbacks).toHaveLength(0);
      expect(device.playbacks).toHaveLength(0);
      expect(readAloudBackStep()).toBeNull();

      readOpenAI();
      for (const section of [-1, 3, 1.5, Number.NaN]) readAloudFromSection(section);
      expect(clips.playbacks[0].calls).toEqual([]);
    });

    it('refuse once the reading cannot be served, before the store reacts', () => {
      for (const read of [readOpenAI, readDevice]) {
        setReadAloudPreference('enabled', true);
        flushSync();
        read();
        const before = clips.playbacks.length + device.playbacks.length;
        setReadAloudPreference('enabled', false);
        readAloudFromSection(1);
        readAloudPreviousSection();
        readAloudNextSection();
        readAloudBack();
        expect(clips.playbacks.length + device.playbacks.length).toBe(before);
        expect(clips.playbacks.flatMap((p) => p.calls)).toEqual([]);
        flushSync();
        expect(readAloudPhase('a')).toBe('idle');
      }
    });
  });

  describe('back', () => {
    it('seeks back within the section through OpenAI, playing or paused, and stays paused', async () => {
      readOpenAI();
      expect(readAloudBackStep()).toBe('seconds');
      clips.playbacks[0].onPiece(0);
      readAloudBack();
      pauseReadAloud();
      readAloudBack();
      expect(readAloudPhase('a')).toBe('paused');
      expect(clips.playbacks[0].calls).toEqual([`seek ${READ_ALOUD_BACK_SECONDS}`, 'pause', `seek ${READ_ALOUD_BACK_SECONDS}`]);
      expect(READ_ALOUD_BACK_SECONDS).toBe(10);
    });

    it('starts the sentence again through a device voice, and does nothing while paused', async () => {
      readDevice();
      expect(readAloudBackStep()).toBe('sentence');
      device.playbacks[0].onPiece(2);
      readAloudBack();
      expect(device.playbacks[0].stopped).toBe(true);
      expect(device.playbacks[1].from).toBe(2);
      pauseReadAloud();
      await settle();
      expect(readAloudBackStep()).toBe('sentence');
      readAloudBack();
      expect(device.playbacks).toHaveLength(2);
      expect(readAloudPhase('a')).toBe('paused');
    });
  });

  describe('a held paused playback', () => {
    const pausedHeld = async (): Promise<void> => {
      readOpenAI();
      clips.playbacks[0].onPiece(1);
      pauseReadAloud();
      await settle();
      expect(clips.playbacks[0].stopped).toBe(false);
    };

    it('is stopped by stopReadAloud', async () => {
      await pausedHeld();
      stopReadAloud();
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(readAloudPhase('a')).toBe('idle');
    });

    it('is stopped by reading another reply', async () => {
      await pausedHeld();
      readOpenAI('b');
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(readAloudPhase('a')).toBe('idle');
      expect(readAloudPhase('b')).toBe('playing');
      // The stopped playback ending late changes nothing of the new one.
      await settle();
      expect(readAloudPhase('b')).toBe('playing');
    });

    it('is stopped when its computer stops offering the voice', async () => {
      await pausedHeld();
      available = false;
      await refreshReadAloudSource(HOME_BACKEND);
      flushSync();
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(readAloudPhase('a')).toBe('idle');
    });

    it('is stopped when read-aloud is turned off', async () => {
      await pausedHeld();
      setReadAloudPreference('enabled', false);
      flushSync();
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(readAloudPhase('a')).toBe('idle');
    });

    it('that ends on its own returns the reading to idle', async () => {
      await pausedHeld();
      clips.playbacks[0].finish('stopped');
      await settle();
      expect(readAloudPhase('a')).toBe('idle');
      resumeReadAloud();
      expect(clips.playbacks[0].calls).toEqual(['pause']);
    });
  });
});
