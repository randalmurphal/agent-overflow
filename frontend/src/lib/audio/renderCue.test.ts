import { describe, expect, it, vi } from 'vitest';
import {
  CUE_FADE_MS,
  CUE_HEADER_BYTES,
  CUE_MAX_SECONDS,
  CUE_PEAK,
  CUE_SAMPLE_RATE,
  TOO_LONG_MESSAGE,
  encodeCueWav,
  renderCueWav,
  type CueAudioEngine,
} from './renderCue';

// The encoder's output is checked against internal/soundlib/wav.go's rules
// rather than against itself: the two are one contract, and a file this side
// writes that the Go side refuses is the only way a user can end up with a
// cue that vanishes on the next listing.
function readHeader(wav: Uint8Array): Record<string, number | string> {
  const view = new DataView(wav.buffer, wav.byteOffset, wav.byteLength);
  const tag = (at: number): string => String.fromCharCode(...wav.subarray(at, at + 4));
  return {
    riff: tag(0),
    riffSize: view.getUint32(4, true),
    wave: tag(8),
    fmtTag: tag(12),
    fmtSize: view.getUint32(16, true),
    format: view.getUint16(20, true),
    channels: view.getUint16(22, true),
    sampleRate: view.getUint32(24, true),
    byteRate: view.getUint32(28, true),
    blockAlign: view.getUint16(32, true),
    bits: view.getUint16(34, true),
    dataTag: tag(36),
    dataSize: view.getUint32(40, true),
  };
}

/** An AudioBuffer stand-in: the three members the renderer actually reads. */
function fakeBuffer(samples: Float32Array, sampleRate = CUE_SAMPLE_RATE): AudioBuffer {
  return {
    duration: samples.length / sampleRate,
    length: samples.length,
    numberOfChannels: 1,
    sampleRate,
    getChannelData: () => samples,
  } as unknown as AudioBuffer;
}

interface EngineStub {
  engine: CueAudioEngine;
  closed: () => number;
  decodedWith: () => ArrayBuffer[];
  renderedFrames: () => number[];
}

/**
 * An engine whose decode answers `decoded` and whose offline render answers
 * `rendered`. happy-dom has no Web Audio at all, which is exactly why
 * renderCue takes its two constructors from the caller.
 */
function stubEngine(options: {
  decoded?: AudioBuffer;
  decodeError?: unknown;
  rendered?: Float32Array;
  renderError?: unknown;
  closeError?: unknown;
}): EngineStub {
  let closes = 0;
  const decodedWith: ArrayBuffer[] = [];
  const renderedFrames: number[] = [];
  const engine: CueAudioEngine = {
    createContext: () =>
      ({
        decodeAudioData: (data: ArrayBuffer) => {
          decodedWith.push(data);
          if (options.decodeError !== undefined) return Promise.reject(options.decodeError);
          return Promise.resolve(options.decoded);
        },
        close: () => {
          closes += 1;
          if (options.closeError !== undefined) return Promise.reject(options.closeError);
          return Promise.resolve();
        },
      }) as unknown as AudioContext,
    createOfflineContext: (frames: number) => {
      renderedFrames.push(frames);
      return {
        destination: {},
        createBufferSource: () => ({ buffer: null, connect: () => {}, start: () => {} }),
        startRendering: () => {
          if (options.renderError !== undefined) return Promise.reject(options.renderError);
          return Promise.resolve(fakeBuffer(options.rendered ?? new Float32Array(0)));
        },
      } as unknown as OfflineAudioContext;
    },
  };
  return {
    engine,
    closed: () => closes,
    decodedWith: () => decodedWith,
    renderedFrames: () => renderedFrames,
  };
}

function cueFile(bytes = 8): File {
  return new File([new Uint8Array(bytes)], 'Desk Bell.mp3', { type: 'audio/mpeg' });
}

describe('encodeCueWav', () => {
  it('writes exactly the header internal/soundlib accepts', () => {
    const wav = encodeCueWav(new Float32Array(1000));

    expect(readHeader(wav)).toEqual({
      riff: 'RIFF',
      riffSize: wav.length - 8,
      wave: 'WAVE',
      fmtTag: 'fmt ',
      fmtSize: 16,
      format: 1,
      channels: 1,
      sampleRate: 44100,
      byteRate: 88200,
      blockAlign: 2,
      bits: 16,
      dataTag: 'data',
      dataSize: 2000,
    });
    expect(wav.length).toBe(CUE_HEADER_BYTES + 2000);
  });

  // 44 bytes of header and nothing else: a trailing LIST/INFO chunk, which
  // most encoders add, is what the Go validator refuses first.
  it('emits no chunk beyond fmt and data', () => {
    const wav = encodeCueWav(new Float32Array(10));
    const view = new DataView(wav.buffer);
    expect(view.getUint32(40, true)).toBe(wav.length - CUE_HEADER_BYTES);
    expect(wav.length % 2).toBe(0);
  });

  // Two's complement has 32768 negative steps and 32767 positive ones. One
  // scale factor for both either clips the positive peak or wastes the
  // negative headroom.
  it('quantises full scale to both 16-bit extremes', () => {
    const wav = encodeCueWav(new Float32Array([1, -1, 0]));
    const view = new DataView(wav.buffer);
    expect(view.getInt16(CUE_HEADER_BYTES, true)).toBe(32767);
    expect(view.getInt16(CUE_HEADER_BYTES + 2, true)).toBe(-32768);
    expect(view.getInt16(CUE_HEADER_BYTES + 4, true)).toBe(0);
  });

  // A sample past full scale would WRAP to the opposite polarity, which is
  // heard as a burst of noise rather than as clipping.
  it('clamps a sample outside [-1, 1] instead of wrapping it', () => {
    const wav = encodeCueWav(new Float32Array([4, -4]));
    const view = new DataView(wav.buffer);
    expect(view.getInt16(CUE_HEADER_BYTES, true)).toBe(32767);
    expect(view.getInt16(CUE_HEADER_BYTES + 2, true)).toBe(-32768);
  });

  it('truncates at the duration cap the backend enforces', () => {
    const wav = encodeCueWav(new Float32Array(CUE_SAMPLE_RATE * (CUE_MAX_SECONDS + 1)));

    expect(wav.length).toBe(CUE_HEADER_BYTES + CUE_MAX_SECONDS * CUE_SAMPLE_RATE * 2);
  });
});

describe('renderCueWav', () => {
  it('normalises, fades and encodes what the engine decoded', async () => {
    const rendered = new Float32Array(CUE_SAMPLE_RATE / 2).fill(0.1);
    const stub = stubEngine({ decoded: fakeBuffer(new Float32Array(rendered.length)), rendered });

    const cue = await renderCueWav(cueFile(), stub.engine);

    expect(cue.seconds).toBeCloseTo(0.5, 5);
    const view = new DataView(cue.wav.buffer);
    // Normalised: the loudest sample sits at CUE_PEAK, not at the 0.1 the
    // engine produced.
    expect(view.getInt16(CUE_HEADER_BYTES, true)).toBe(Math.round(CUE_PEAK * 0x7fff));
    // Faded: the last sample of the tail ramp is silent, so the file cannot
    // end on a discontinuity.
    expect(view.getInt16(cue.wav.length - 2, true)).toBe(0);
    const fadeFrames = Math.round((CUE_FADE_MS / 1000) * CUE_SAMPLE_RATE);
    const beforeFade = cue.wav.length - (fadeFrames + 1) * 2;
    expect(view.getInt16(beforeFade, true)).toBe(Math.round(CUE_PEAK * 0x7fff));
  });

  // decodeAudioData DETACHES the buffer it is handed. Passing the caller's
  // own ArrayBuffer would leave it zero-length, so a retry would decode
  // nothing and the file would read as empty.
  it('decodes a copy, leaving the file bytes intact', async () => {
    const rendered = new Float32Array(10).fill(0.2);
    const stub = stubEngine({ decoded: fakeBuffer(new Float32Array(10)), rendered });
    const file = cueFile(64);

    await renderCueWav(file, stub.engine);

    expect(stub.decodedWith()).toHaveLength(1);
    expect(stub.decodedWith()[0].byteLength).toBe(64);
    expect(new Uint8Array(await file.arrayBuffer())).toHaveLength(64);
  });

  it('renders offline at the canonical rate, into one channel', async () => {
    const decoded = fakeBuffer(new Float32Array(22050), 22050);
    const stub = stubEngine({ decoded, rendered: new Float32Array(44100).fill(0.5) });

    await renderCueWav(cueFile(), stub.engine);

    // One second at 22050 Hz becomes 44100 frames at the canonical rate.
    expect(stub.renderedFrames()).toEqual([44100]);
  });

  it('refuses a file the engine cannot decode, naming the reason', async () => {
    const stub = stubEngine({ decodeError: new Error('Unable to decode audio data') });

    await expect(renderCueWav(cueFile(), stub.engine)).rejects.toThrow(
      'That file could not be decoded as audio (Unable to decode audio data).',
    );
  });

  it('refuses a file past the duration cap', async () => {
    const decoded = fakeBuffer(new Float32Array(CUE_SAMPLE_RATE * (CUE_MAX_SECONDS + 1)));
    const stub = stubEngine({ decoded });

    await expect(renderCueWav(cueFile(), stub.engine)).rejects.toThrow(TOO_LONG_MESSAGE);
  });

  it('refuses a file that decodes to no audio', async () => {
    const stub = stubEngine({ decoded: fakeBuffer(new Float32Array(0)) });

    await expect(renderCueWav(cueFile(), stub.engine)).rejects.toThrow('holds no audio');
  });

  it('reports a failed offline render rather than writing a silent cue', async () => {
    const stub = stubEngine({
      decoded: fakeBuffer(new Float32Array(100)),
      renderError: new Error('rendering failed'),
    });

    await expect(renderCueWav(cueFile(), stub.engine)).rejects.toThrow(
      'That file could not be converted (rendering failed).',
    );
  });

  // An AudioContext holds a hardware output stream open, so it is closed on
  // every path — including the one where the decode threw.
  it('closes the decoding context whether the decode succeeded or failed', async () => {
    const ok = stubEngine({
      decoded: fakeBuffer(new Float32Array(10)),
      rendered: new Float32Array(10).fill(0.3),
    });
    await renderCueWav(cueFile(), ok.engine);
    expect(ok.closed()).toBe(1);

    const bad = stubEngine({ decodeError: new Error('nope') });
    await expect(renderCueWav(cueFile(), bad.engine)).rejects.toThrow();
    expect(bad.closed()).toBe(1);
  });

  // A context the engine already tore down rejects its close. That is not a
  // reason to fail a render that produced a usable cue.
  it('survives a context that refuses to close', async () => {
    const stub = stubEngine({
      decoded: fakeBuffer(new Float32Array(10)),
      rendered: new Float32Array(10).fill(0.3),
      closeError: new Error('already closed'),
    });

    await expect(renderCueWav(cueFile(), stub.engine)).resolves.toMatchObject({
      seconds: 10 / CUE_SAMPLE_RATE,
    });
  });

  // Pure silence is a legal canonical file. Refusing it here would invent a
  // rule internal/soundlib does not have, and dividing by the zero peak
  // would produce NaN samples.
  it('leaves silence alone rather than dividing by a zero peak', async () => {
    const stub = stubEngine({
      decoded: fakeBuffer(new Float32Array(100)),
      rendered: new Float32Array(100),
    });

    const cue = await renderCueWav(cueFile(), stub.engine);
    expect(cue.wav.subarray(CUE_HEADER_BYTES).every((byte) => byte === 0)).toBe(true);
  });

  // The production path takes its constructors from the page. A browser
  // without Web Audio must say so, not fail with a TypeError on undefined.
  it('explains an engine with no Web Audio at all', async () => {
    vi.stubGlobal('AudioContext', undefined);
    vi.stubGlobal('OfflineAudioContext', undefined);
    try {
      await expect(renderCueWav(cueFile())).rejects.toThrow('cannot decode audio files');
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
