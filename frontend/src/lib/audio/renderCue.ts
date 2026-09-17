// Turning a file the user picked into a canonical notification cue.
//
// THE SECURITY POSTURE IS THE WHOLE DESIGN. Nothing the user hands this app is
// stored or played as handed. The file's bytes go to `decodeAudioData` — the
// browser engine's own sandboxed decoder, which either yields PCM samples or
// throws — and everything past that point works on Float32 samples, never on
// the original container. What is written to disk is a file THIS module
// encoded, in one shape internal/soundlib re-checks byte by byte before it is
// saved and again on every listing.
//
// So the pipeline is deliberately lossy in both directions: mixed to mono,
// resampled to 44100 Hz, normalised, faded, quantised to 16-bit. None of the
// original file's structure survives it, which is the point.
//
// The encoder half is pure and takes Float32 samples, so it is testable
// without an audio engine; the decode half takes its two contexts from the
// caller's environment, so a test can hand it stubs.

/** The canonical cue geometry. internal/soundlib/wav.go refuses anything else. */
export const CUE_SAMPLE_RATE = 44100;
export const CUE_CHANNELS = 1;
export const CUE_BITS_PER_SAMPLE = 16;
/** The header this module writes: RIFF + `fmt ` + `data`, and nothing else. */
export const CUE_HEADER_BYTES = 44;
/** The duration cap, matched to soundlib.MaxSoundSeconds. */
export const CUE_MAX_SECONDS = 3;

/**
 * Loudness the render normalises to: the RMS of the loudest CUE_LOUDNESS_WINDOW_S
 * of audio, in dBFS. The same measure and the same target the built-in cues
 * are generated to (scripts/gen-notification-sounds.py LOUDNESS_DB), which
 * sits between the Windows and macOS default notification sounds. Peak
 * normalisation was tried first and rejected: a sharp click and a sustained
 * tone at the same peak differ by 10 dB in how loud they are heard, so a
 * custom cue could land far louder than the built-in it replaced.
 */
export const CUE_LOUDNESS_DB = -17;
export const CUE_LOUDNESS_WINDOW_S = 0.1;
/** Hard ceiling after normalisation, so a very peaky file cannot clip. */
export const CUE_CEILING = 0.85;

/**
 * Linear fade applied to the tail.
 *
 * A file cut mid-waveform ends on a discontinuity, which every speaker
 * reproduces as a click. 12 ms is long enough to remove it and short enough
 * that it is not heard as a fade.
 */
export const CUE_FADE_MS = 12;

/** The message a file longer than the cap is refused with. */
export const TOO_LONG_MESSAGE = 'Keep it under 3 seconds';

export interface RenderedCue {
  /** The canonical WAV file. */
  wav: Uint8Array;
  /** Its duration, for the caller to show. */
  seconds: number;
}

/**
 * The two engine constructors this module needs, injected so the pure half can
 * be tested without an audio engine. Production callers omit it and get the
 * globals.
 */
export interface CueAudioEngine {
  createContext: () => AudioContext;
  createOfflineContext: (frames: number) => OfflineAudioContext;
}

function defaultEngine(): CueAudioEngine {
  if (typeof AudioContext !== 'function' || typeof OfflineAudioContext !== 'function') {
    throw new Error('This browser cannot decode audio files.');
  }
  return {
    createContext: () => new AudioContext(),
    createOfflineContext: (frames) => new OfflineAudioContext(CUE_CHANNELS, frames, CUE_SAMPLE_RATE),
  };
}

/**
 * Decode `file` and render it to a canonical cue.
 *
 * Throws with a message meant for the user on every failure path: a file the
 * engine cannot decode, a file past the duration cap, an engine that cannot
 * render. The caller shows the message; nothing here is console-only.
 */
export async function renderCueWav(file: File, engine?: CueAudioEngine): Promise<RenderedCue> {
  const audio = engine ?? defaultEngine();
  const source = await file.arrayBuffer();

  const context = audio.createContext();
  let decoded: AudioBuffer;
  try {
    // A copy, because decodeAudioData DETACHES the buffer it is given: the
    // caller's ArrayBuffer would be zero-length afterwards, and a retry would
    // decode nothing.
    decoded = await context.decodeAudioData(source.slice(0));
  } catch (cause) {
    throw new Error(`That file could not be decoded as audio (${errorText(cause)}).`);
  } finally {
    // An AudioContext holds a hardware output stream open. Closing is
    // best-effort: a context the engine already tore down rejects, and that is
    // not a reason to fail a render that succeeded.
    void Promise.resolve(context.close()).catch(() => {});
  }

  if (decoded.duration > CUE_MAX_SECONDS) {
    throw new Error(TOO_LONG_MESSAGE);
  }
  if (decoded.length === 0) {
    throw new Error('That file holds no audio.');
  }

  const samples = await resampleToMono(decoded, audio);
  normalise(samples);
  fadeTail(samples);
  return { wav: encodeCueWav(samples), seconds: samples.length / CUE_SAMPLE_RATE };
}

/**
 * Mix every channel down and resample to the canonical rate in one pass.
 *
 * OfflineAudioContext does both: connecting an AudioBuffer to a one-channel
 * destination mixes, and rendering at 44100 Hz resamples with the engine's own
 * filter. Hand-written resampling would be a second-rate copy of code already
 * in the process.
 */
async function resampleToMono(decoded: AudioBuffer, audio: CueAudioEngine): Promise<Float32Array> {
  const frames = Math.ceil(decoded.duration * CUE_SAMPLE_RATE);
  if (frames <= 0) throw new Error('That file holds no audio.');
  const offline = audio.createOfflineContext(frames);
  const source = offline.createBufferSource();
  source.buffer = decoded;
  source.connect(offline.destination);
  source.start();
  let rendered: AudioBuffer;
  try {
    rendered = await offline.startRendering();
  } catch (cause) {
    throw new Error(`That file could not be converted (${errorText(cause)}).`);
  }
  // A copy, not the engine's view: the rendered buffer's storage belongs to a
  // context this function is done with, and the samples are mutated in place
  // by the two steps that follow.
  return Float32Array.from(rendered.getChannelData(0));
}

/**
 * Scale the samples so their loudest CUE_LOUDNESS_WINDOW_S sits at
 * CUE_LOUDNESS_DB, then clamp to CUE_CEILING.
 *
 * Silence is left alone rather than divided by zero — a cue of pure silence is
 * a legal canonical file, and refusing it here would be inventing a rule the
 * backend does not have.
 */
function normalise(samples: Float32Array): void {
  const loudest = loudestWindowRms(samples);
  if (loudest === 0) return;
  const gain = 10 ** (CUE_LOUDNESS_DB / 20) / loudest;
  for (let i = 0; i < samples.length; i += 1) {
    samples[i] = Math.max(-CUE_CEILING, Math.min(CUE_CEILING, samples[i] * gain));
  }
}

/**
 * RMS of the loudest CUE_LOUDNESS_WINDOW_S slice, stepped a tenth of a window
 * at a time. A file shorter than one window is measured whole.
 */
export function loudestWindowRms(samples: Float32Array): number {
  const window = Math.min(samples.length, Math.max(1, Math.round(CUE_LOUDNESS_WINDOW_S * CUE_SAMPLE_RATE)));
  const hop = Math.max(1, Math.floor(window / 10));
  let loudest = 0;
  for (let start = 0; start + window <= samples.length; start += hop) {
    let sum = 0;
    for (let i = start; i < start + window; i += 1) sum += samples[i] * samples[i];
    loudest = Math.max(loudest, Math.sqrt(sum / window));
  }
  return loudest;
}

/** Ramp the last CUE_FADE_MS linearly to zero so the file cannot end on a click. */
function fadeTail(samples: Float32Array): void {
  const fadeFrames = Math.min(samples.length, Math.round((CUE_FADE_MS / 1000) * CUE_SAMPLE_RATE));
  if (fadeFrames <= 1) return;
  const start = samples.length - fadeFrames;
  for (let i = 0; i < fadeFrames; i += 1) {
    samples[start + i] *= 1 - i / (fadeFrames - 1);
  }
}

/**
 * Encode Float32 samples as the canonical 16-bit PCM WAV.
 *
 * Pure, and the one place the byte layout is written: the 44-byte header, then
 * samples, then nothing. Exported so a test can assert the layout against the
 * rules internal/soundlib enforces without running an audio engine.
 */
export function encodeCueWav(samples: Float32Array): Uint8Array {
  const blockAlign = (CUE_CHANNELS * CUE_BITS_PER_SAMPLE) / 8;
  const frames = Math.min(samples.length, CUE_MAX_SECONDS * CUE_SAMPLE_RATE);
  const dataBytes = frames * blockAlign;
  const out = new Uint8Array(CUE_HEADER_BYTES + dataBytes);
  const view = new DataView(out.buffer);

  writeTag(out, 0, 'RIFF');
  view.setUint32(4, out.length - 8, true);
  writeTag(out, 8, 'WAVE');
  writeTag(out, 12, 'fmt ');
  view.setUint32(16, 16, true); // PCM fmt chunk, no extension block
  view.setUint16(20, 1, true); // format tag: uncompressed PCM
  view.setUint16(22, CUE_CHANNELS, true);
  view.setUint32(24, CUE_SAMPLE_RATE, true);
  view.setUint32(28, CUE_SAMPLE_RATE * blockAlign, true); // byte rate
  view.setUint16(32, blockAlign, true);
  view.setUint16(34, CUE_BITS_PER_SAMPLE, true);
  writeTag(out, 36, 'data');
  view.setUint32(40, dataBytes, true);

  for (let i = 0; i < frames; i += 1) {
    // Clamp before scaling: a sample outside [-1, 1] (which normalisation
    // cannot produce but a caller could pass) would wrap around to the
    // opposite polarity and be heard as a burst of noise.
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    // Asymmetric scaling because two's complement is: 32767 positive steps,
    // 32768 negative ones. Using one factor for both either clips the
    // positive peak or wastes the negative headroom.
    view.setInt16(
      CUE_HEADER_BYTES + i * blockAlign,
      Math.round(clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff),
      true,
    );
  }
  return out;
}

function writeTag(out: Uint8Array, offset: number, tag: string): void {
  for (let i = 0; i < tag.length; i += 1) out[offset + i] = tag.charCodeAt(i);
}

function errorText(cause: unknown): string {
  if (cause instanceof Error && cause.message !== '') return cause.message;
  return String(cause);
}
