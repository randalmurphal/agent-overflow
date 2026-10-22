// Microphone samples at the audio context's rate, made into what live
// dictation sends: 24 kHz 16-bit signed little-endian mono, in 100 ms
// pieces. Pure, so the AudioWorklet that runs it (pcmCapture.worklet.ts)
// is a thin shell and this is tested directly.

/** The rate live dictation sends; the computer's transcriber takes only
 * this. */
export const LIVE_SAMPLE_RATE = 24_000;

/** The AudioWorklet processor that runs this (pcmCapture.worklet.ts),
 * named here so the page need not import the worklet module. */
export const PCM_CAPTURE_PROCESSOR = 'ao-pcm-capture';

/** Samples per piece: 100 ms. */
export const PIECE_SAMPLES = LIVE_SAMPLE_RATE / 10;

/**
 * Averages input samples down to LIVE_SAMPLE_RATE: each output sample is the
 * mean of the inputs that fall in its span, which also filters out what the
 * lower rate cannot carry. A lower input rate repeats samples instead.
 */
export class PcmDownsampler {
  private readonly step: number;
  private phase = 0;
  private sum = 0;
  private count = 0;
  private piece = new Int16Array(PIECE_SAMPLES);
  private filled = 0;
  private readonly emit: (piece: Int16Array) => void;

  constructor(inputRate: number, emit: (piece: Int16Array) => void) {
    if (!(inputRate > 0)) throw new Error(`invalid input sample rate ${inputRate}`);
    this.step = LIVE_SAMPLE_RATE / inputRate;
    this.emit = emit;
  }

  /** Takes one block of input samples, -1 to 1. */
  push(samples: Float32Array): void {
    for (const sample of samples) {
      this.sum += sample;
      this.count += 1;
      this.phase += this.step;
      while (this.phase >= 1) {
        this.phase -= 1;
        this.write(this.sum / this.count);
        if (this.phase < 1) {
          this.sum = 0;
          this.count = 0;
        }
      }
    }
  }

  /** Emits the samples of a part-filled piece. */
  flush(): void {
    if (this.filled === 0) return;
    this.emit(this.piece.slice(0, this.filled));
    this.piece = new Int16Array(PIECE_SAMPLES);
    this.filled = 0;
  }

  private write(value: number): void {
    const clamped = Math.max(-1, Math.min(1, value));
    this.piece[this.filled] = clamped < 0 ? Math.round(clamped * 0x8000) : Math.round(clamped * 0x7fff);
    this.filled += 1;
    if (this.filled === PIECE_SAMPLES) {
      this.emit(this.piece);
      this.piece = new Int16Array(PIECE_SAMPLES);
      this.filled = 0;
    }
  }
}
