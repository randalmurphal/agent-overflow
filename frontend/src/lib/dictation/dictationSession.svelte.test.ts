import { describe, expect, it, vi } from 'vitest';
import { MAX_RECORDING_MS } from './dictationSession.svelte';
import { dictationFixture as fixture } from '../../test/helpers/dictation';

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
