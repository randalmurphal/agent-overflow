// Read-aloud through a computer's OpenAI voices: which voice a reading
// takes by computer and saved or overriding voice, the wait for the first
// clip, a source that goes away mid-reading, pause and resume keeping the
// reading's choice, a thread whose computer stops being verified, and what
// a failure says.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { SpeechEngine, SpeechOptions, SpeechOutcome } from '../native/speech';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { fakeSpeechEngine } from '../../test/helpers/speech';
import { hasScope } from '../transport/scopes';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend, getPinnedBackend } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { clipSpeechEngine, type ClipAudio } from '../native/clipSpeech';
import {
  pauseReadAloud,
  readAloud,
  readAloudAvailable,
  readAloudPhase,
  readAloudRoute,
  readAloudWaiting,
  resetReadAloudForTest,
  restartReadAloud,
  resumeReadAloud,
  toggleReadAloud,
  useOpenAIEngineForTest,
  watchReadAloudVoices,
} from './readAloud.svelte';
import { reloadReadAloudPreferencesForTest, setReadAloudPreference } from './readAloudPreferences.svelte';
import {
  READ_ALOUD_OFF_CODE,
  readAloudClipFetcher,
  readAloudSourceVoices,
  refreshReadAloudSource,
  resetReadAloudSourcesForTest,
} from './readAloudSource.svelte';
import * as toast from './toast.svelte';

vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => true) }));

const VOICES = [{ id: 'marin', name: 'Marin' }, { id: 'cedar', name: 'Cedar' }];
const REPLY = 'First sentence. Second sentence. Third sentence.';
// Replies of a thread on each computer, and of one no computer verifiably
// owns (its owner is unknown and several are attached).
const LAPTOP = { thread: 'on-laptop' };
const DESK = { thread: 'on-desk' };
const NOBODY = { thread: 'unowned' };

interface ClipPlayback {
  backend: string;
  pieces: string[];
  from: number;
  options: SpeechOptions;
  onPiece: (index: number) => void;
  stopped: boolean;
  finish: (outcome: SpeechOutcome) => void;
  fail: (err: unknown) => void;
}

/** OpenAI engines per computer that, like the real one, report a sentence
 * only when the test says it sounds. */
function fakeClipEngines() {
  const playbacks: ClipPlayback[] = [];
  const factory = (backend: string): SpeechEngine => ({
    voices: async () => [],
    onVoicesChanged: () => () => undefined,
    speak: (pieces, from, options, onPiece) => {
      let finish!: (outcome: SpeechOutcome) => void;
      let fail!: (err: unknown) => void;
      const done = new Promise<SpeechOutcome>((resolve, reject) => {
        finish = resolve;
        fail = reject;
      });
      const record: ClipPlayback = { backend, pieces, from, options, onPiece, stopped: false, finish, fail };
      playbacks.push(record);
      return {
        done,
        stop: () => {
          record.stopped = true;
          finish('stopped');
        },
      };
    },
  });
  return { playbacks, factory };
}

const fakeClient = {
  callByID: async () => null, callByName: async () => null, subscribe: () => () => undefined,
  installStepUpProver: () => undefined, setLease: () => undefined, setWatchedThreads: () => undefined,
  getStatus: () => ({ status: 'connected', nextAttemptAt: null }), onStatusChange: () => () => undefined,
  getHello: () => null, onHelloChange: () => () => undefined, onReplay: () => () => undefined, close: () => undefined,
};

function attach(id: string, backendId: string): void {
  __attachBackendForTest({
    id, backendId, name: id,
    wsUrl: `ws://localhost:3000/ws/backend/${id}`, bootstrapUrl: `/bootstrap/${id}.json`,
  }, fakeClient as never);
}

// What each computer answers to ReadAloudSource.
const available = new Map<string, boolean>();

async function offer(backend: string, on: boolean): Promise<void> {
  available.set(backend, on);
  await refreshReadAloudSource(backend);
  flushSync();
}

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await Promise.resolve();
  flushSync();
}

describe('read-aloud in OpenAI voices', () => {
  let device: ReturnType<typeof fakeSpeechEngine>;
  let clips: ReturnType<typeof fakeClipEngines>;

  async function boot(deviceVoices: number): Promise<void> {
    resetReadAloudForTest();
    device = fakeSpeechEngine(deviceVoices);
    watchReadAloudVoices(device.engine);
    clips = fakeClipEngines();
    useOpenAIEngineForTest(clips.factory);
    await settle();
  }

  beforeEach(async () => {
    resetBindingMocks();
    resetReadAloudSourcesForTest();
    available.clear();
    attach('laptop', '99999999-8888-4777-8666-555555555555');
    attach('desk', '11111111-2222-4333-8444-555555555555');
    noteThread(LAPTOP.thread, 'laptop', 1);
    noteThread(DESK.thread, 'desk', 1);
    setBindingMock('ReadAloudSource', async () => ({ available: available.get(getPinnedBackend() ?? '') ?? false, voices: VOICES }));
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    setReadAloudPreference('enabled', true);
    await boot(1);
    await offer('laptop', true);
    await offer('desk', false);
  });

  afterEach(() => {
    resetReadAloudForTest();
    resetReadAloudSourcesForTest();
    __resetBackendsForTest();
    __resetEntityIndexForTest();
    vi.unstubAllGlobals();
    vi.mocked(hasScope).mockReset().mockReturnValue(true);
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    vi.restoreAllMocks();
  });

  it('reads a saved OpenAI voice through the computer that offers it, and the device voice elsewhere', () => {
    setReadAloudPreference('voiceId', 'openai:cedar');
    setReadAloudPreference('rate', 1.5);
    readAloud('a', REPLY, LAPTOP);
    expect(clips.playbacks[0]).toMatchObject({
      backend: 'laptop',
      pieces: ['First sentence.', 'Second sentence.', 'Third sentence.'],
      from: 0,
      options: { voiceId: 'cedar', rate: 1.5, volume: 1 },
    });
    expect(device.playbacks).toHaveLength(0);

    // The desk is off: its reply is read in the device's default voice.
    readAloud('b', REPLY, DESK);
    expect(clips.playbacks[0].stopped).toBe(true);
    expect(device.playbacks[0].options.voiceId).toBe('');
    // No computer: the device's voice.
    readAloud('c', REPLY, NOBODY);
    expect(device.playbacks[1].options.voiceId).toBe('');
    expect(clips.playbacks).toHaveLength(1);
  });

  it('reads a saved device voice on the device even where OpenAI voices are offered', () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('a', REPLY, LAPTOP);
    expect(device.playbacks[0].options.voiceId).toBe('v0');
    expect(clips.playbacks).toHaveLength(0);
  });

  it('reads a saved OpenAI voice the computer does not list in the device’s default voice', () => {
    setReadAloudPreference('voiceId', 'openai:ash');
    readAloud('a', REPLY, LAPTOP);
    expect(device.playbacks[0].options.voiceId).toBe('');
    expect(clips.playbacks).toHaveLength(0);
  });

  it('on a device with no voices, reads through the computer’s saved or first OpenAI voice', async () => {
    await boot(0);
    expect(readAloudAvailable('laptop')).toBe(true);
    expect(readAloudAvailable('desk')).toBe(false);
    expect(readAloudAvailable(null)).toBe(false);

    readAloud('a', REPLY, LAPTOP);
    expect(clips.playbacks[0].options.voiceId).toBe('marin');
    setReadAloudPreference('voiceId', 'openai:cedar');
    readAloud('a', REPLY, LAPTOP);
    expect(clips.playbacks[1].options.voiceId).toBe('cedar');
    setReadAloudPreference('voiceId', 'v3');
    readAloud('a', REPLY, LAPTOP);
    expect(clips.playbacks[2].options.voiceId).toBe('marin');

    readAloud('b', REPLY, DESK);
    expect(readAloudRoute('desk', 'openai:cedar')).toBeNull();
    expect(clips.playbacks).toHaveLength(3);
    expect(readAloudPhase('b')).toBe('idle');
  });

  it('takes an overriding voice for one reading without changing the saved one', () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('preview', REPLY, LAPTOP, 'openai:cedar');
    expect(clips.playbacks[0].options.voiceId).toBe('cedar');
    readAloud('a', REPLY, LAPTOP);
    expect(device.playbacks[0].options.voiceId).toBe('v0');

    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('b', REPLY, LAPTOP, 'v0');
    expect(device.playbacks[1].options.voiceId).toBe('v0');
  });

  it('waits from the start of a reading until its first sentence sounds', async () => {
    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('a', REPLY, LAPTOP);
    expect(readAloudPhase('a')).toBe('playing');
    expect(readAloudWaiting('a')).toBe(true);
    expect(readAloudWaiting('b')).toBe(false);
    clips.playbacks[0].onPiece(0);
    expect(readAloudWaiting('a')).toBe(false);

    // Resume waits again for its sentence; a device reading never waits.
    pauseReadAloud();
    await settle();
    expect(readAloudWaiting('a')).toBe(false);
    resumeReadAloud();
    expect(readAloudWaiting('a')).toBe(true);
    readAloud('b', REPLY, NOBODY);
    expect(readAloudWaiting('b')).toBe(false);
  });

  it('pause, resume and restart keep the reading’s computer and voice', async () => {
    setReadAloudPreference('voiceId', 'openai:cedar');
    readAloud('a', REPLY, LAPTOP);
    clips.playbacks[0].onPiece(0);
    clips.playbacks[0].onPiece(1);
    pauseReadAloud();
    expect(clips.playbacks[0].stopped).toBe(true);
    await settle();
    expect(readAloudPhase('a')).toBe('paused');

    setReadAloudPreference('voiceId', 'v0');
    resumeReadAloud();
    expect(clips.playbacks[1]).toMatchObject({ backend: 'laptop', from: 1, options: { voiceId: 'cedar' } });
    expect(toggleReadAloud()).toBe(true);
    await settle();
    restartReadAloud();
    expect(clips.playbacks[2]).toMatchObject({ backend: 'laptop', from: 0, options: { voiceId: 'cedar' } });
    expect(device.playbacks).toHaveLength(0);
  });

  it('stops silently when the computer stops offering the voice, playing or paused', async () => {
    const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('a', REPLY, LAPTOP);
    await offer('laptop', false);
    expect(clips.playbacks[0].stopped).toBe(true);
    expect(readAloudPhase('a')).toBe('idle');

    await offer('laptop', true);
    readAloud('a', REPLY, LAPTOP);
    pauseReadAloud();
    await settle();
    await offer('laptop', false);
    expect(readAloudPhase('a')).toBe('idle');
    await offer('laptop', true);
    expect(toggleReadAloud()).toBe(false);

    // A detached computer is gone the same way.
    readAloud('a', REPLY, LAPTOP);
    detachBackend('laptop');
    flushSync();
    expect(clips.playbacks.at(-1)!.stopped).toBe(true);
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).not.toHaveBeenCalled();
  });

  it('a device reading is not stopped by a computer going away', async () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('a', REPLY, LAPTOP);
    await offer('laptop', false);
    expect(device.playbacks[0].stopped).toBe(false);
    expect(readAloudPhase('a')).toBe('playing');
  });

  it('says the reason at once when a reading fails for another cause, and asks the computer nothing', async () => {
    const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
    const reads = setBindingMock('ReadAloudSource', async () => ({ available: true, voices: VOICES }));
    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('a', REPLY, LAPTOP);
    const failure = new Error('OpenAI did not read the text as written');
    clips.playbacks[0].fail(failure);
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).toHaveBeenCalledWith('Read aloud stopped: OpenAI did not read the text as written', failure);
    expect(reads).not.toHaveBeenCalled();

    readAloud('b', REPLY, NOBODY);
    const voiceFailure = new Error('speech failed: synthesis-failed');
    device.playbacks[0].fail(voiceFailure);
    await settle();
    expect(shown).toHaveBeenLastCalledWith('Read aloud stopped: this device’s voice failed.', voiceFailure);
  });

  describe('a thread whose computer stops being verified', () => {
    it('stops a playing OpenAI reading when another computer claims the thread or it moves', async () => {
      const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
      setReadAloudPreference('voiceId', 'openai:marin');
      readAloud('a', REPLY, LAPTOP);
      clips.playbacks[0].onPiece(0);
      expect(clips.playbacks[0].backend).toBe('laptop');

      // Both computers claim it at one epoch.
      noteThread(LAPTOP.thread, 'desk', 1);
      flushSync();
      expect(clips.playbacks[0].stopped).toBe(true);
      expect(readAloudPhase('a')).toBe('idle');

      // It moves to a computer that offers the voice too.
      await offer('desk', true);
      noteThread(DESK.thread, 'desk', 1);
      readAloud('b', REPLY, DESK);
      expect(clips.playbacks[1].backend).toBe('desk');
      noteThread(DESK.thread, 'laptop', 2);
      flushSync();
      expect(clips.playbacks[1].stopped).toBe(true);
      expect(readAloudPhase('b')).toBe('idle');
      await settle();
      expect(shown).not.toHaveBeenCalled();
    });

    it('stops a paused OpenAI reading and resumes nothing on the old computer', async () => {
      setReadAloudPreference('voiceId', 'openai:marin');
      readAloud('a', REPLY, LAPTOP);
      clips.playbacks[0].onPiece(1);
      pauseReadAloud();
      await settle();
      expect(readAloudPhase('a')).toBe('paused');

      noteThread(LAPTOP.thread, 'desk', 1);
      // Before the store reacts, resume and restart refuse on their own.
      resumeReadAloud();
      restartReadAloud();
      expect(clips.playbacks).toHaveLength(1);
      flushSync();
      expect(readAloudPhase('a')).toBe('idle');
      expect(toggleReadAloud()).toBe(false);
      expect(clips.playbacks).toHaveLength(1);
    });

    it('leaves a device-voice reading playing and resumable', async () => {
      setReadAloudPreference('voiceId', 'v0');
      readAloud('a', REPLY, LAPTOP);
      noteThread(LAPTOP.thread, 'desk', 1);
      flushSync();
      expect(device.playbacks[0].stopped).toBe(false);
      expect(readAloudPhase('a')).toBe('playing');
      pauseReadAloud();
      await settle();
      resumeReadAloud();
      expect(device.playbacks[1].options.voiceId).toBe('v0');
      expect(readAloudPhase('a')).toBe('playing');
    });

    it('leaves a reading that names its computer, as Settings > Voice does, alone', async () => {
      readAloud('preview', REPLY, { computer: 'laptop' }, 'openai:cedar');
      noteThread(LAPTOP.thread, 'desk', 1);
      noteThread(NOBODY.thread, 'laptop', 1);
      noteThread(NOBODY.thread, 'desk', 1);
      flushSync();
      expect(clips.playbacks[0].stopped).toBe(false);
      expect(readAloudPhase('preview')).toBe('playing');
      pauseReadAloud();
      await settle();
      resumeReadAloud();
      expect(clips.playbacks[1]).toMatchObject({ backend: 'laptop', options: { voiceId: 'cedar' } });
    });
  });

  // The real clip engine and fetcher against a scripted mint and HTTP
  // route, so what is tested is what the computer answers on the wire.
  describe('a failure the computer answers', () => {
    const SWITCHED_OFF = 'read aloud in OpenAI voices is off on this computer';

    function silentAudio(): ClipAudio {
      return {
        src: '', volume: 1, playbackRate: 1, defaultPlaybackRate: 1, preservesPitch: true,
        play: () => new Promise<void>(() => undefined),
        pause: () => undefined, load: () => undefined, removeAttribute: () => undefined,
        addEventListener: () => undefined, removeEventListener: () => undefined,
      };
    }

    function wire(mint: () => string | Promise<string>, route: () => Response): { reads: ReturnType<typeof setBindingMock>; fetches: string[] } {
      const audio = silentAudio();
      useOpenAIEngineForTest((backend) => clipSpeechEngine({
        voices: () => readAloudSourceVoices(backend).map((voice) => ({ id: voice.id, name: voice.name, lang: '', isDefault: false })),
        fetchClip: readAloudClipFetcher(backend),
        audio: () => audio,
        urls: { create: () => 'blob:clip', revoke: () => undefined },
      }));
      setBindingMock('MintReadAloudTicket', mint);
      const fetches: string[] = [];
      vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
        fetches.push(String(input));
        return route();
      });
      // The computer offers the voice again by the time it is asked: the
      // switch went off and straight back on.
      const reads = setBindingMock('ReadAloudSource', async () => ({ available: true, voices: VOICES }));
      return { reads, fetches };
    }

    async function readUntilStopped(): Promise<void> {
      setReadAloudPreference('voiceId', 'openai:marin');
      readAloud('a', REPLY, LAPTOP);
      await vi.waitFor(() => expect(readAloudPhase('a')).toBe('idle'), { timeout: 1000 });
      await settle();
    }

    it('stops quietly and asks the computer again when the mint says the switch is off', async () => {
      const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
      const { reads, fetches } = wire(() => { throw new TransportError(READ_ALOUD_OFF_CODE, SWITCHED_OFF); }, () => new Response('', { status: 500 }));
      await readUntilStopped();
      expect(fetches).toEqual([]);
      expect(reads).toHaveBeenCalledTimes(1);
      expect(shown).not.toHaveBeenCalled();
    });

    it('stops quietly and asks the computer again when the speech route answers 403', async () => {
      const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
      const { reads, fetches } = wire(() => '/attachments/speech/s1?ticket=t', () => new Response(SWITCHED_OFF, { status: 403 }));
      await readUntilStopped();
      expect(fetches).toEqual(['/backend/laptop/attachments/speech/s1?ticket=t']);
      expect(reads).toHaveBeenCalledTimes(1);
      expect(shown).not.toHaveBeenCalled();
      expect(readAloudRoute('laptop', 'openai:marin')).toMatchObject({ kind: 'openai', backend: 'laptop' });
    });

    it('says the reason of any other refusal at once and asks the computer nothing', async () => {
      const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
      for (const [status, body] of [
        [409, 'the key file is readable by other users; run chmod 600 on it'],
        [502, 'OpenAI did not read the text as written'],
        [404, 'this sentence was already read aloud or has expired'],
      ] as const) {
        shown.mockClear();
        const { reads } = wire(() => '/attachments/speech/s1?ticket=t', () => new Response(body, { status }));
        await readUntilStopped();
        expect(shown).toHaveBeenCalledTimes(1);
        expect(shown.mock.calls[0][0]).toBe(`Read aloud stopped: ${body}`);
        expect(shown.mock.calls[0][1]).toMatchObject({ status });
        expect(reads).not.toHaveBeenCalled();
      }
    });
  });
});
