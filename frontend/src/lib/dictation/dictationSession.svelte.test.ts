import { describe, expect, it, vi } from 'vitest';
import { MAX_LIVE_APPEND_BYTES, MAX_RECORDING_MS } from './dictationSession.svelte';
import { FakePcm, SWITCHED_OFF, dictationFixture as fixture } from '../../test/helpers/dictation';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Lets every settled promise's callbacks run. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

const live = (options: Parameters<typeof fixture>[0] = {}) => fixture({ source: 'live', ...options });

/** The bytes of each live append, in call order. */
function appended(f: ReturnType<typeof fixture>): number[][] {
  return f.remote.live.append.mock.calls.map((call) => Array.from(call[2] as Uint8Array));
}

describe('DictationSession', () => {
  it('records, uploads every piece in order, and inserts the transcript', async () => {
    const f = fixture();
    await f.session.start();
    expect(f.session.phase).toBe('recording');
    expect(f.remote.begin).toHaveBeenCalledWith('audio/webm');
    expect(f.recorder().timeslice).toBe(1000);

    f.recorder().emit('a');
    f.recorder().emit('b');
    f.tick();
    expect(f.session.level).toBe(4);

    await f.session.stop();
    expect(f.remote.upload.mock.calls.map((call) => [call[0], call[1]])).toEqual([
      ['rec-1', 0], ['rec-1', 1], ['rec-1', 2],
    ]);
    expect(f.remote.finish).toHaveBeenCalledWith('rec-1', 3);
    expect(f.inserted).toEqual(['hello world']);
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBeNull();
    expect(f.session.level).toBe(0);
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.level.closed).toBe(true);
    expect(f.timers()).toBe(0);
  });

  it('quiets this device’s speech before it opens the microphone', async () => {
    let quietedBeforeOpening = false;
    const f = fixture({
      openMicrophone: async () => {
        quietedBeforeOpening = f.quieted.count === 1;
        return { getTracks: () => [] } as unknown as MediaStream;
      },
    });
    await f.session.start();
    expect(quietedBeforeOpening).toBe(true);
  });

  it('never uploads a later piece after one fails, and reports the failure', async () => {
    const f = fixture();
    f.remote.upload.mockRejectedValueOnce(new Error('ticket refused'));
    await f.session.start();
    f.recorder().emit('a');
    f.recorder().emit('b');
    f.tick();
    await f.session.stop();
    expect(f.remote.upload).toHaveBeenCalledTimes(1);
    expect(f.remote.finish).not.toHaveBeenCalled();
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
    expect(f.session.error).toBe('ticket refused');
    expect(f.inserted).toEqual([]);
  });

  it('skips transcription when nothing louder than silence was heard', async () => {
    const f = fixture();
    f.level.value = 0.001;
    await f.session.start();
    f.recorder().emit('a');
    f.tick();
    await f.session.stop();
    expect(f.remote.finish).not.toHaveBeenCalled();
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
    expect(f.session.error).toBe('No speech heard.');
  });

  it('transcribes without a level meter rather than guessing silence', async () => {
    const f = fixture({ meter: false });
    await f.session.start();
    await f.session.stop();
    expect(f.remote.finish).toHaveBeenCalledWith('rec-1', 1);
    expect(f.inserted).toEqual(['hello world']);
  });

  it('reports an empty transcript and inserts nothing', async () => {
    const f = fixture();
    f.remote.finish.mockResolvedValueOnce('  ');
    await f.session.start();
    f.tick();
    await f.session.stop();
    expect(f.inserted).toEqual([]);
    expect(f.session.error).toBe('No speech heard.');
  });

  it('reports a transcript that could not be inserted', async () => {
    const f = fixture({ insert: false });
    await f.session.start();
    f.tick();
    await f.session.stop();
    expect(f.session.error).toBe('The transcript could not be inserted.');
  });

  it('cancel while recording drops the recording and releases the microphone', async () => {
    const f = fixture();
    await f.session.start();
    f.recorder().emit('a');
    f.session.cancel();
    expect(f.session.phase).toBe('idle');
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.timers()).toBe(0);
    await Promise.resolve();
    expect(f.remote.finish).not.toHaveBeenCalled();
  });

  it('cancel while transcribing drops the late transcript', async () => {
    const f = fixture();
    let answer!: (text: string) => void;
    f.remote.finish.mockReturnValueOnce(new Promise<string>((resolve) => { answer = resolve; }));
    await f.session.start();
    f.tick();
    const stopping = f.session.stop();
    await vi.waitFor(() => expect(f.remote.finish).toHaveBeenCalled());
    f.session.cancel();
    answer('too late');
    await stopping;
    expect(f.inserted).toEqual([]);
    expect(f.session.phase).toBe('idle');
    // The computer is told to stop transcribing, not left to finish.
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
  });

  it('releases the microphone at stop, before the uploads finish', async () => {
    const f = fixture();
    let land!: () => void;
    f.remote.upload.mockReturnValueOnce(new Promise<void>((resolve) => { land = resolve; }));
    await f.session.start();
    f.recorder().emit('a');
    f.tick();
    const stopping = f.session.stop();
    await vi.waitFor(() => expect(f.tracks[0].stop).toHaveBeenCalled());
    expect(f.remote.finish).not.toHaveBeenCalled();
    land();
    await stopping;
    expect(f.inserted).toEqual(['hello world']);
  });

  it("an abandoned stop never touches the next attempt's microphone or uploads", async () => {
    const f = fixture();
    // A's first piece hangs until released; its later pieces fail.
    let landA!: () => void;
    const heldA = new Promise<void>((resolve) => { landA = resolve; });
    let piecesA = 0;
    f.remote.upload.mockImplementation(async (recordingId: string) => {
      if (recordingId !== 'rec-1') return;
      if (piecesA++ === 0) return heldA;
      throw new Error('late failure of A');
    });
    await f.session.start();
    f.recorder().emit('a');
    f.tick();
    const stoppingA = f.session.stop();
    await vi.waitFor(() => expect(f.tracks[0].stop).toHaveBeenCalled());
    f.session.cancel();

    f.remote.begin.mockResolvedValueOnce('rec-2');
    await f.session.start();
    expect(f.session.phase).toBe('recording');
    landA();
    await stoppingA;

    expect(f.tracks[1].stop).not.toHaveBeenCalled();
    expect(f.session.phase).toBe('recording');
    expect(f.timers()).toBe(2);
    f.recorder().emit('b');
    f.tick();
    await f.session.stop();
    expect(f.remote.finish).toHaveBeenCalledTimes(1);
    expect(f.remote.finish).toHaveBeenLastCalledWith('rec-2', 2);
    expect(f.session.error).toBeNull();
  });

  it('closes the recording on the computer when the recorder cannot start', async () => {
    const f = fixture({ createRecorder: () => { throw new Error('recorder refused'); } });
    await f.session.start();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('recorder refused');
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.timers()).toBe(0);
    f.session.dispose();
    expect(f.remote.cancel).toHaveBeenCalledTimes(1);
  });

  it('cancel while the microphone opens closes it when it arrives', async () => {
    let grant!: (stream: MediaStream) => void;
    const track = { stop: vi.fn() };
    const f = fixture({ openMicrophone: () => new Promise((resolve) => { grant = resolve; }) });
    const starting = f.session.start();
    expect(f.session.phase).toBe('starting');
    f.session.cancel();
    grant({ getTracks: () => [track] } as unknown as MediaStream);
    await starting;
    expect(track.stop).toHaveBeenCalled();
    expect(f.remote.begin).not.toHaveBeenCalled();
    expect(f.session.phase).toBe('idle');
  });

  it('cancel while the computer opens the recording cancels it there', async () => {
    const f = fixture();
    let open!: (id: string) => void;
    f.remote.begin.mockReturnValueOnce(new Promise<string>((resolve) => { open = resolve; }));
    const starting = f.session.start();
    await vi.waitFor(() => expect(f.remote.begin).toHaveBeenCalled());
    f.session.cancel();
    open('rec-late');
    await starting;
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-late');
    expect(f.tracks[0].stop).toHaveBeenCalled();
  });

  it('names a blocked microphone and a missing one', async () => {
    const blocked = fixture({ openMicrophone: () => Promise.reject(Object.assign(new Error('x'), { name: 'NotAllowedError' })) });
    await blocked.session.start();
    expect(blocked.session.error).toBe('Microphone access is blocked.');
    expect(blocked.session.phase).toBe('idle');
    const missing = fixture({ openMicrophone: () => Promise.reject(Object.assign(new Error('x'), { name: 'NotFoundError' })) });
    await missing.session.start();
    expect(missing.session.error).toBe('No microphone found.');
  });

  it('closes the microphone when the computer refuses to open a recording', async () => {
    const f = fixture();
    f.remote.begin.mockRejectedValueOnce(new Error('Codex is not signed in with ChatGPT.'));
    await f.session.start();
    expect(f.session.error).toBe('Codex is not signed in with ChatGPT.');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.timers()).toBe(0);
  });

  it('says so when the engine cannot record', async () => {
    const f = fixture({ format: null });
    await f.session.start();
    expect(f.session.error).toBe('This browser cannot record audio.');
    expect(f.session.phase).toBe('idle');
  });

  it('stops itself at the length cap and transcribes', async () => {
    const f = fixture();
    await f.session.start();
    f.tick();
    expect(MAX_RECORDING_MS).toBe(10 * 60 * 1000);
    f.fireCap();
    await vi.waitFor(() => expect(f.inserted).toEqual(['hello world']));
  });

  it('a new attempt clears the last error and toggle ignores the in-between phases', async () => {
    const f = fixture();
    f.remote.begin.mockRejectedValueOnce(new Error('first failed'));
    await f.session.start();
    expect(f.session.error).toBe('first failed');
    f.session.toggle();
    expect(f.session.error).toBeNull();
    expect(f.session.phase).toBe('starting');
    f.session.toggle();
    await vi.waitFor(() => expect(f.session.phase).toBe('recording'));
    expect(f.remote.begin).toHaveBeenCalledTimes(2);
  });

  it('dispose releases an open recording', async () => {
    const f = fixture();
    await f.session.start();
    f.session.dispose();
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.remote.cancel).toHaveBeenCalledWith('rec-1');
  });
});

describe('DictationSession, live', () => {
  it('sends what was said before the computer connected, in order, then finishes and inserts', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    await f.session.start();
    expect(f.session.phase).toBe('recording');
    f.pcm().emit(1, 2);
    f.pcm().emit(3, 4);
    f.tick();
    expect(f.remote.live.append).not.toHaveBeenCalled();

    connecting.resolve('live-1');
    await settle();
    expect(appended(f)).toEqual([[1, 2, 3, 4]]);
    expect(f.session.heard).toBe('heard 0');

    await f.session.stop();
    // The flushed tail goes after everything before it.
    expect(f.remote.live.append.mock.calls.map((call) => [call[0], call[1]])).toEqual([['live-1', 0], ['live-1', 1]]);
    expect(appended(f)[1]).toEqual([9, 9]);
    expect(f.remote.live.finish).toHaveBeenCalledWith('live-1');
    expect(f.inserted).toEqual(['hello live']);
    expect(f.session.phase).toBe('idle');
    expect(f.session.heard).toBe('');
    expect(f.remote.live.cancel).not.toHaveBeenCalled();
    expect(f.remote.begin).not.toHaveBeenCalled();
    expect(f.pcm().closed).toBe(true);
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.level.closed).toBe(true);
    expect(f.timers()).toBe(0);
  });

  it('keeps one append in flight and joins what arrives meanwhile', async () => {
    const first = deferred<string>();
    const f = live();
    f.remote.live.append.mockReturnValueOnce(first.promise);
    await f.session.start();
    await settle();
    f.pcm().emit(1);
    f.pcm().emit(2);
    f.pcm().emit(3);
    expect(appended(f)).toEqual([[1]]);
    first.resolve('heard 0');
    await settle();
    expect(appended(f)).toEqual([[1], [2, 3]]);
    expect(f.remote.live.append.mock.calls.map((call) => call[1])).toEqual([0, 1]);
  });

  it('never joins past one append’s worth', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    await f.session.start();
    const half = MAX_LIVE_APPEND_BYTES / 2;
    for (let i = 0; i < 3; i++) f.pcm().onPiece?.(new Uint8Array(half).fill(i));
    connecting.resolve('live-1');
    await settle();
    const sizes = f.remote.live.append.mock.calls.map((call) => (call[2] as Uint8Array).length);
    expect(sizes).toEqual([MAX_LIVE_APPEND_BYTES, half]);
    expect((f.remote.live.append.mock.calls[1][2] as Uint8Array)[0]).toBe(2);
  });

  it('ends the recording at once when the computer cannot connect', async () => {
    const f = live();
    f.remote.live.begin.mockRejectedValueOnce(new Error('OpenAI refused the API key'));
    await f.session.start();
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('OpenAI refused the API key');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.pcm().closed).toBe(true);
    expect(f.timers()).toBe(0);
    expect(f.remote.live.cancel).not.toHaveBeenCalled();
    f.pcm().emit(1);
    expect(f.remote.live.append).not.toHaveBeenCalled();
  });

  it('ends the recording at once when an append fails, and drops it on the computer', async () => {
    const f = live();
    f.remote.live.append.mockRejectedValueOnce(new Error('live transcription: rate limited'));
    await f.session.start();
    await settle();
    f.pcm().emit(1);
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('live transcription: rate limited');
    expect(f.remote.live.cancel).toHaveBeenCalledWith('live-1');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    f.pcm().emit(2);
    expect(f.remote.live.append).toHaveBeenCalledTimes(1);
    expect(f.remote.live.finish).not.toHaveBeenCalled();
  });

  it('stops without an error when the failure was live being switched off', async () => {
    const f = live();
    f.remote.live.append.mockRejectedValueOnce(new Error('live dictation with the OpenAI API key is off on this computer'));
    f.remote.live.stillOn.mockResolvedValueOnce(false);
    await f.session.start();
    await settle();
    f.pcm().emit(1);
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBeNull();
    expect(f.session.activeSource).toBeNull();
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.remote.live.cancel).toHaveBeenCalledWith('live-1');
  });

  it('a switch-off found while stopping also ends without an error', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    f.remote.live.stillOn.mockResolvedValueOnce(false);
    await f.session.start();
    f.pcm().emit(1);
    f.tick();
    const stopping = f.session.stop();
    connecting.reject(new Error('live dictation with the OpenAI API key is off on this computer'));
    await stopping;
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBeNull();
    expect(f.remote.live.finish).not.toHaveBeenCalled();
    expect(f.inserted).toEqual([]);
  });

  it('a finish ended by the switch going off ends without an error', async () => {
    const finishing = deferred<string>();
    const f = live();
    f.remote.live.finish.mockReturnValueOnce(finishing.promise);
    f.remote.live.stillOn.mockResolvedValueOnce(false);
    await f.session.start();
    await settle();
    f.pcm().emit(0, 0x40);
    f.tick();
    const stopping = f.session.stop();
    await settle();
    expect(f.remote.live.finish).toHaveBeenCalled();
    finishing.reject(new Error('dictation: live dictation was switched off, so this recording ended'));
    await stopping;
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBeNull();
    expect(f.inserted).toEqual([]);
  });

  it('a finish that fails while live is still on shows why', async () => {
    const f = live();
    f.remote.live.finish.mockRejectedValueOnce(new Error('OpenAI could not transcribe the turn'));
    await f.session.start();
    await settle();
    f.pcm().emit(0, 0x40);
    f.tick();
    await f.session.stop();
    await settle();
    expect(f.remote.live.stillOn).toHaveBeenCalled();
    expect(f.session.error).toBe('OpenAI could not transcribe the turn');
  });

  it('a recording the switch ended stays quiet even when live is back on by now', async () => {
    for (const where of ['connect', 'append', 'finish'] as const) {
      const f = live();
      f.remote.live.stillOn.mockResolvedValue(true);
      if (where === 'connect') f.remote.live.begin.mockRejectedValueOnce(new Error(SWITCHED_OFF));
      if (where === 'append') f.remote.live.append.mockRejectedValueOnce(new Error(SWITCHED_OFF));
      if (where === 'finish') f.remote.live.finish.mockRejectedValueOnce(new Error(SWITCHED_OFF));
      await f.session.start();
      await settle();
      f.pcm().emit(0, 0x40);
      f.tick();
      await settle();
      if (where === 'finish') await f.session.stop();
      await settle();
      expect(f.session.phase, where).toBe('idle');
      expect(f.session.error, where).toBeNull();
      expect(f.remote.live.stillOn, where).not.toHaveBeenCalled();
      expect(f.inserted, where).toEqual([]);
    }
  });

  it('shows the failure when live cannot be confirmed off', async () => {
    const f = live();
    f.remote.live.begin.mockRejectedValueOnce(new Error('could not reach OpenAI'));
    f.remote.live.stillOn.mockRejectedValueOnce(new Error('offline'));
    await f.session.start();
    await settle();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('could not reach OpenAI');
  });

  it('a failure while transcribing settles once, with the failure', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    await f.session.start();
    f.pcm().emit(1);
    f.tick();
    const stopping = f.session.stop();
    expect(f.session.phase).toBe('transcribing');
    connecting.reject(new Error('could not reach OpenAI'));
    await stopping;
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('could not reach OpenAI');
    expect(f.remote.live.finish).not.toHaveBeenCalled();
    expect(f.inserted).toEqual([]);
  });

  it('stop waits for the connection and the last append before finishing', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    await f.session.start();
    f.pcm().emit(1);
    f.tick();
    const stopping = f.session.stop();
    await settle();
    expect(f.remote.live.finish).not.toHaveBeenCalled();
    connecting.resolve('live-1');
    await stopping;
    expect(appended(f)).toEqual([[1, 9, 9]]);
    expect(f.remote.live.finish).toHaveBeenCalledTimes(1);
    expect(f.remote.live.append.mock.invocationCallOrder[0]).toBeLessThan(f.remote.live.finish.mock.invocationCallOrder[0]);
    expect(f.inserted).toEqual(['hello live']);
  });

  it('a capture that cannot flush inserts what it sent and says the ending may be missing', async () => {
    const f = live();
    await f.session.start();
    await settle();
    f.pcm().flushFails = true;
    f.pcm().emit(0, 0x40);
    f.tick();
    await f.session.stop();
    expect(appended(f)).toEqual([[0, 0x40]]);
    expect(f.pcm().closed).toBe(true);
    expect(f.inserted).toEqual(['hello live']);
    expect(f.session.error).toBe('The end of the recording may be missing: The microphone capture did not stop in time.');
    // The next attempt starts clean.
    await f.session.start();
    expect(f.session.error).toBeNull();
  });

  it('cancel while the capture opens closes it when it arrives and opens nothing else', async () => {
    const opening = deferred<FakePcm>();
    const f = live({ openPcm: () => opening.promise });
    const starting = f.session.start();
    await settle();
    expect(f.session.activeSource).toBe('live');
    f.session.cancel();
    expect(f.session.activeSource).toBeNull();
    expect(f.tracks[0].stop).toHaveBeenCalled();
    const pcm = new FakePcm();
    opening.resolve(pcm);
    await starting;
    expect(pcm.closed).toBe(true);
    expect(pcm.onPiece).toBeNull();
    expect(f.level.closed).toBe(false);
    expect(f.timers()).toBe(0);
    expect(f.remote.live.begin).not.toHaveBeenCalled();
    expect(f.session.phase).toBe('idle');
  });

  it('names the source of the attempt in progress, and none once it ends', async () => {
    const f = live();
    expect(f.session.activeSource).toBeNull();
    await f.session.start();
    expect(f.session.activeSource).toBe('live');
    f.pcm().emit(1);
    f.tick();
    await f.session.stop();
    expect(f.session.activeSource).toBeNull();

    const recorded = fixture();
    await recorded.session.start();
    expect(recorded.session.activeSource).toBe('recorded');
    recorded.session.cancel();
    expect(recorded.session.activeSource).toBeNull();
  });

  it('skips the transcript and drops the session when only silence was sent, whatever the meter read', async () => {
    const f = live();
    f.level.value = 1;
    await f.session.start();
    await settle();
    f.pcm().tail = new Uint8Array([0, 0]);
    // Just under SILENCE_PEAK: 600 / 32768.
    f.pcm().emit(0x58, 0x02, 0xa8, 0xfd);
    f.tick();
    await f.session.stop();
    expect(f.remote.live.finish).not.toHaveBeenCalled();
    expect(f.remote.live.cancel).toHaveBeenCalledWith('live-1');
    expect(f.session.error).toBe('No speech heard.');
  });

  it('hears a sound the meter missed between its reads', async () => {
    const f = live();
    f.level.value = 0;
    await f.session.start();
    await settle();
    f.pcm().tail = null;
    // One loud negative sample among silence: -16384.
    f.pcm().emit(0, 0, 0x00, 0xc0, 0, 0);
    f.tick();
    await f.session.stop();
    expect(f.remote.live.finish).toHaveBeenCalled();
    expect(f.inserted).toEqual(['hello live']);
  });

  it('cancel while connecting drops the session once it opens and sends nothing', async () => {
    const connecting = deferred<string>();
    const f = live();
    f.remote.live.begin.mockReturnValueOnce(connecting.promise);
    await f.session.start();
    f.pcm().emit(1);
    f.session.cancel();
    expect(f.session.phase).toBe('idle');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    connecting.resolve('live-1');
    await settle();
    expect(f.remote.live.cancel).toHaveBeenCalledWith('live-1');
    expect(f.remote.live.append).not.toHaveBeenCalled();
  });

  it('cancel while transcribing drops the late transcript and the session', async () => {
    const finishing = deferred<string>();
    const f = live();
    f.remote.live.finish.mockReturnValueOnce(finishing.promise);
    await f.session.start();
    await settle();
    f.pcm().emit(1);
    f.tick();
    const stopping = f.session.stop();
    await settle();
    expect(f.remote.live.finish).toHaveBeenCalled();
    f.session.cancel();
    expect(f.remote.live.cancel).toHaveBeenCalledWith('live-1');
    finishing.resolve('too late');
    await stopping;
    expect(f.inserted).toEqual([]);
    expect(f.session.phase).toBe('idle');
  });

  it('closes the microphone when the capture cannot open', async () => {
    const f = live({ openPcm: async () => { throw new Error('AudioWorklet module failed to load'); } });
    await f.session.start();
    expect(f.session.phase).toBe('idle');
    expect(f.session.error).toBe('AudioWorklet module failed to load');
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(f.remote.live.begin).not.toHaveBeenCalled();
  });

  it('says so when the engine cannot stream, and opens nothing', async () => {
    const f = live({ streamsPcm: false });
    await f.session.start();
    expect(f.session.error).toBe('This browser cannot stream audio for live dictation.');
    expect(f.tracks).toHaveLength(0);
    expect(f.remote.live.begin).not.toHaveBeenCalled();
  });

  it('a late heard text from an abandoned attempt never reaches the next one', async () => {
    const append = deferred<string>();
    const f = live();
    f.remote.live.append.mockReturnValueOnce(append.promise);
    await f.session.start();
    await settle();
    f.pcm().emit(1);
    f.session.cancel();
    await f.session.start();
    await settle();
    append.resolve('from the first attempt');
    await settle();
    expect(f.session.heard).toBe('');
    expect(f.session.phase).toBe('recording');
  });
});
