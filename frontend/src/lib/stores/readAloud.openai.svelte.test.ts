// Read-aloud through a computer's OpenAI voices: which voice a reading
// takes by computer and saved or overriding voice, the wait for the first
// clip, a source that goes away mid-reading, pause and resume keeping the
// reading's choice, and what a failure says.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { SpeechEngine, SpeechOptions, SpeechOutcome } from '../native/speech';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { fakeSpeechEngine } from '../../test/helpers/speech';
import { hasScope } from '../transport/scopes';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend, getPinnedBackend } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
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
import { refreshReadAloudSource, resetReadAloudSourcesForTest } from './readAloudSource.svelte';
import * as toast from './toast.svelte';

vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => true) }));

const VOICES = [{ id: 'marin', name: 'Marin' }, { id: 'cedar', name: 'Cedar' }];
const REPLY = 'First sentence. Second sentence. Third sentence.';

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
    vi.mocked(hasScope).mockReset().mockReturnValue(true);
    localStorage.clear();
    reloadReadAloudPreferencesForTest();
    vi.restoreAllMocks();
  });

  it('reads a saved OpenAI voice through the computer that offers it, and the device voice elsewhere', () => {
    setReadAloudPreference('voiceId', 'openai:cedar');
    setReadAloudPreference('rate', 1.5);
    readAloud('a', REPLY, 'laptop');
    expect(clips.playbacks[0]).toMatchObject({
      backend: 'laptop',
      pieces: ['First sentence.', 'Second sentence.', 'Third sentence.'],
      from: 0,
      options: { voiceId: 'cedar', rate: 1.5, volume: 1 },
    });
    expect(device.playbacks).toHaveLength(0);

    // The desk is off: its reply is read in the device's default voice.
    readAloud('b', REPLY, 'desk');
    expect(clips.playbacks[0].stopped).toBe(true);
    expect(device.playbacks[0].options.voiceId).toBe('');
    // No computer: the device's voice.
    readAloud('c', REPLY, null);
    expect(device.playbacks[1].options.voiceId).toBe('');
    expect(clips.playbacks).toHaveLength(1);
  });

  it('reads a saved device voice on the device even where OpenAI voices are offered', () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('a', REPLY, 'laptop');
    expect(device.playbacks[0].options.voiceId).toBe('v0');
    expect(clips.playbacks).toHaveLength(0);
  });

  it('reads a saved OpenAI voice the computer does not list in the device’s default voice', () => {
    setReadAloudPreference('voiceId', 'openai:ash');
    readAloud('a', REPLY, 'laptop');
    expect(device.playbacks[0].options.voiceId).toBe('');
    expect(clips.playbacks).toHaveLength(0);
  });

  it('on a device with no voices, reads through the computer’s saved or first OpenAI voice', async () => {
    await boot(0);
    expect(readAloudAvailable('laptop')).toBe(true);
    expect(readAloudAvailable('desk')).toBe(false);
    expect(readAloudAvailable(null)).toBe(false);

    readAloud('a', REPLY, 'laptop');
    expect(clips.playbacks[0].options.voiceId).toBe('marin');
    setReadAloudPreference('voiceId', 'openai:cedar');
    readAloud('a', REPLY, 'laptop');
    expect(clips.playbacks[1].options.voiceId).toBe('cedar');
    setReadAloudPreference('voiceId', 'v3');
    readAloud('a', REPLY, 'laptop');
    expect(clips.playbacks[2].options.voiceId).toBe('marin');

    readAloud('b', REPLY, 'desk');
    expect(readAloudRoute('desk', 'openai:cedar')).toBeNull();
    expect(clips.playbacks).toHaveLength(3);
    expect(readAloudPhase('b')).toBe('idle');
  });

  it('takes an overriding voice for one reading without changing the saved one', () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('preview', REPLY, 'laptop', 'openai:cedar');
    expect(clips.playbacks[0].options.voiceId).toBe('cedar');
    readAloud('a', REPLY, 'laptop');
    expect(device.playbacks[0].options.voiceId).toBe('v0');

    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('b', REPLY, 'laptop', 'v0');
    expect(device.playbacks[1].options.voiceId).toBe('v0');
  });

  it('waits from the start of a reading until its first sentence sounds', async () => {
    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('a', REPLY, 'laptop');
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
    readAloud('b', REPLY, null);
    expect(readAloudWaiting('b')).toBe(false);
  });

  it('pause, resume and restart keep the reading’s computer and voice', async () => {
    setReadAloudPreference('voiceId', 'openai:cedar');
    readAloud('a', REPLY, 'laptop');
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
    readAloud('a', REPLY, 'laptop');
    await offer('laptop', false);
    expect(clips.playbacks[0].stopped).toBe(true);
    expect(readAloudPhase('a')).toBe('idle');

    await offer('laptop', true);
    readAloud('a', REPLY, 'laptop');
    pauseReadAloud();
    await settle();
    await offer('laptop', false);
    expect(readAloudPhase('a')).toBe('idle');
    await offer('laptop', true);
    expect(toggleReadAloud()).toBe(false);

    // A detached computer is gone the same way.
    readAloud('a', REPLY, 'laptop');
    detachBackend('laptop');
    flushSync();
    expect(clips.playbacks.at(-1)!.stopped).toBe(true);
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).not.toHaveBeenCalled();
  });

  it('a device reading is not stopped by a computer going away', async () => {
    setReadAloudPreference('voiceId', 'v0');
    readAloud('a', REPLY, 'laptop');
    await offer('laptop', false);
    expect(device.playbacks[0].stopped).toBe(false);
    expect(readAloudPhase('a')).toBe('playing');
  });

  it('says the computer’s reason when a reading fails, and stops silently when its switch went off', async () => {
    const shown = vi.spyOn(toast, 'addErrorToast').mockImplementation(() => undefined as never);
    setReadAloudPreference('voiceId', 'openai:marin');
    readAloud('a', REPLY, 'laptop');
    const failure = new Error('OpenAI did not read the text as written');
    clips.playbacks[0].fail(failure);
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).toHaveBeenCalledWith('Read aloud stopped: OpenAI did not read the text as written', failure);

    const reads = setBindingMock('ReadAloudSource', async () => ({ available: false, voices: VOICES }));
    readAloud('a', REPLY, 'laptop');
    clips.playbacks[1].fail(new TransportError('read_aloud_api_off', 'Read aloud in OpenAI voices is off on this computer; turn it on in Settings > Voice.'));
    await settle();
    expect(readAloudPhase('a')).toBe('idle');
    expect(shown).toHaveBeenCalledTimes(1);
    // The computer is asked again, so the voice stops being offered.
    await vi.waitFor(() => expect(reads).toHaveBeenCalled());
    await vi.waitFor(() => expect(readAloudRoute('laptop', 'openai:marin')).toEqual({ kind: 'device', voiceId: '' }));

    readAloud('b', REPLY, null);
    const voiceFailure = new Error('speech failed: synthesis-failed');
    device.playbacks[0].fail(voiceFailure);
    await settle();
    expect(shown).toHaveBeenLastCalledWith('Read aloud stopped: this device’s voice failed.', voiceFailure);
  });
});
