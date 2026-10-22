import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { nativeSpeechEngineForTest, webSpeechEngine } from './speech';
import type { SpeechPlugin } from './plugins';

// A speechSynthesis that speaks nothing: the test ends or fails each
// utterance, and cancel() interrupts the one speaking the way browsers do.
class FakeSynth extends EventTarget {
  spoken: SpeechSynthesisUtterance[] = [];
  queue: SpeechSynthesisUtterance[] = [];
  cancels = 0;
  voiceList = [{ voiceURI: 'urn:alex', name: 'Alex', lang: 'en-US', default: true }] as SpeechSynthesisVoice[];
  getVoices(): SpeechSynthesisVoice[] {
    return this.voiceList;
  }
  speak(utterance: SpeechSynthesisUtterance): void {
    this.spoken.push(utterance);
    this.queue.push(utterance);
  }
  cancel(): void {
    this.cancels += 1;
    for (const utterance of this.queue.splice(0)) {
      utterance.onerror?.({ error: 'interrupted' } as SpeechSynthesisErrorEvent);
    }
  }
  end(): void {
    this.queue.shift()?.onend?.({} as SpeechSynthesisEvent);
  }
  error(error: string): void {
    this.queue.shift()?.onerror?.({ error } as SpeechSynthesisErrorEvent);
  }
}

class FakeUtterance {
  text: string;
  voice: SpeechSynthesisVoice | null = null;
  rate = 1;
  volume = 1;
  onend: ((event: SpeechSynthesisEvent) => void) | null = null;
  onerror: ((event: SpeechSynthesisErrorEvent) => void) | null = null;
  constructor(text: string) {
    this.text = text;
  }
}

const OPTIONS = { voiceId: 'urn:alex', rate: 1.25, volume: 0.5 };

describe('webSpeechEngine', () => {
  beforeEach(() => {
    vi.stubGlobal('SpeechSynthesisUtterance', FakeUtterance);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('lists the browser’s voices', async () => {
    const engine = webSpeechEngine(new FakeSynth() as unknown as SpeechSynthesis);
    expect(await engine.voices()).toEqual([{ id: 'urn:alex', name: 'Alex', lang: 'en-US', isDefault: true }]);
  });

  it('speaks each piece in turn from the starting index with the chosen voice, rate and volume', async () => {
    const synth = new FakeSynth();
    const engine = webSpeechEngine(synth as unknown as SpeechSynthesis);
    const started: number[] = [];
    const playback = engine.speak(['a', 'b', 'c'], 1, OPTIONS, (i) => started.push(i));
    synth.end();
    synth.end();
    expect(await playback.done).toBe('ended');
    expect(synth.spoken.map((u) => u.text)).toEqual(['b', 'c']);
    expect(started).toEqual([1, 2]);
    expect(synth.spoken[0]).toMatchObject({ rate: 1.25, volume: 0.5 });
    expect(synth.spoken[0].voice?.voiceURI).toBe('urn:alex');
  });

  it('reads its own cancel as stopped, not as a failure', async () => {
    const synth = new FakeSynth();
    const engine = webSpeechEngine(synth as unknown as SpeechSynthesis);
    const playback = engine.speak(['a', 'b'], 0, OPTIONS, () => undefined);
    playback.stop();
    expect(await playback.done).toBe('stopped');
    expect(synth.spoken).toHaveLength(1);
  });

  it('rejects when the voice fails', async () => {
    const synth = new FakeSynth();
    const engine = webSpeechEngine(synth as unknown as SpeechSynthesis);
    const playback = engine.speak(['a'], 0, OPTIONS, () => undefined);
    synth.error('synthesis-failed');
    await expect(playback.done).rejects.toThrow('synthesis-failed');
  });

  it('stops the playing reading when another starts', async () => {
    const synth = new FakeSynth();
    const engine = webSpeechEngine(synth as unknown as SpeechSynthesis);
    const first = engine.speak(['a', 'b'], 0, OPTIONS, () => undefined);
    const second = engine.speak(['c'], 0, OPTIONS, () => undefined);
    expect(await first.done).toBe('stopped');
    synth.end();
    expect(await second.done).toBe('ended');
    expect(synth.spoken.map((u) => u.text)).toEqual(['a', 'c']);
  });
});

describe('the Android engine', () => {
  function fakePlugin() {
    const listeners = new Map<string, Array<(event: never) => void>>();
    const calls: Array<{ method: string; options: unknown }> = [];
    const plugin = {
      voices: async () => ({ voices: [{ id: 'en-us-x-1', name: 'English', lang: 'en-US', isDefault: true }] }),
      speak: async (options: unknown) => {
        calls.push({ method: 'speak', options });
      },
      stop: async (options: unknown) => {
        calls.push({ method: 'stop', options });
      },
      addListener: async (event: string, handler: (event: never) => void) => {
        listeners.set(event, [...(listeners.get(event) ?? []), handler]);
        return {
          remove: async () => {
            listeners.set(event, (listeners.get(event) ?? []).filter((h) => h !== handler));
          },
        };
      },
    };
    const emit = (event: string, payload: unknown) => {
      for (const handler of listeners.get(event) ?? []) handler(payload as never);
    };
    return { plugin: plugin as unknown as SpeechPlugin, calls, emit, listeners };
  }

  async function settle(): Promise<void> {
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
  }

  it('queues the pieces in one session and follows its progress to the end', async () => {
    const fake = fakePlugin();
    const engine = nativeSpeechEngineForTest(fake.plugin);
    const started: number[] = [];
    const playback = engine.speak(['a', 'b'], 1, OPTIONS, (i) => started.push(i));
    await settle();
    const session = (fake.calls[0].options as { session: string }).session;
    expect(fake.calls[0].options).toEqual({ session, pieces: ['a', 'b'], from: 1, ...OPTIONS });
    fake.emit('progress', { session: 'other', index: 0 });
    fake.emit('progress', { session, index: 1 });
    fake.emit('done', { session, outcome: 'ended' });
    expect(await playback.done).toBe('ended');
    expect(started).toEqual([1]);
    await settle();
    expect(fake.listeners.get('progress')).toEqual([]);
  });

  it('stops its own session and ignores what that session reports afterwards', async () => {
    const fake = fakePlugin();
    const engine = nativeSpeechEngineForTest(fake.plugin);
    const started: number[] = [];
    const playback = engine.speak(['a', 'b'], 0, OPTIONS, (i) => started.push(i));
    await settle();
    const session = (fake.calls[0].options as { session: string }).session;
    playback.stop();
    expect(await playback.done).toBe('stopped');
    await settle();
    expect(fake.calls[1]).toEqual({ method: 'stop', options: { session } });
    fake.emit('progress', { session, index: 1 });
    expect(started).toEqual([]);
  });

  it('rejects when the session fails', async () => {
    const fake = fakePlugin();
    const engine = nativeSpeechEngineForTest(fake.plugin);
    const playback = engine.speak(['a'], 0, OPTIONS, () => undefined);
    await settle();
    const session = (fake.calls[0].options as { session: string }).session;
    fake.emit('done', { session, outcome: 'error', message: 'engine unavailable' });
    await expect(playback.done).rejects.toThrow('engine unavailable');
  });
});
