// The browser half of a DictationSession: microphone, MediaRecorder, the
// live PCM capture and a loudness meter, as the engine provides them.

import { isNativeShell } from '../native/platform';
import { pauseReadAloud } from '../stores/readAloud.svelte';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { DictationEnvironment, LevelMeter, PcmStream, RecorderLike, RecordingFormat } from './dictationSession.svelte';
import { PCM_CAPTURE_PROCESSOR } from './pcmDownsampler';
import pcmCaptureUrl from './pcmCapture.worklet.ts?worker&url';

/** Speech needs little: 32 kbps Opus is about 2.4 MB for ten minutes. */
const AUDIO_BITS_PER_SECOND = 32_000;

/** Chromium and Firefox stream WebM pieces this often while recording. */
const WEBM_TIMESLICE_MS = 1000;

/** How long stopping waits for the capture's last piece. The worklet
 * answers within a render quantum; this bounds a context the engine has
 * stopped running. */
const FLUSH_TIMEOUT_MS = 2000;

/** A finished recording is decoded at this rate to be measured. Opus
 * decodes at it, so nothing recorded is filtered out before it is judged. */
const LOUDNESS_SAMPLE_RATE = 48_000;

/** A decode the engine has not finished by now is given up on. */
const LOUDNESS_DEADLINE_MS = 5000;

type DecodedAudio = Pick<AudioBuffer, 'numberOfChannels' | 'sampleRate' | 'getChannelData'>;

async function decodeRecording(recording: Blob): Promise<AudioBuffer> {
  const context = new OfflineAudioContext(1, 1, LOUDNESS_SAMPLE_RATE);
  return context.decodeAudioData(await recording.arrayBuffer());
}

/** The loudest stretch of decoded audio, 0 to 1: the highest RMS of
 * `stretchSeconds` starting at any sample, so the verdict does not depend
 * on where a sound falls. A recording shorter than a stretch is measured
 * over a whole one, so a few samples cannot stand for a loud moment. */
export function loudestStretch(audio: DecodedAudio, stretchSeconds: number): number {
  const block = Math.max(1, Math.round(audio.sampleRate * stretchSeconds));
  let loudest = 0;
  for (let channel = 0; channel < audio.numberOfChannels; channel++) {
    const samples = audio.getChannelData(channel);
    let sum = 0;
    for (let i = 0; i < samples.length; i++) {
      sum += samples[i] * samples[i];
      if (i >= block) sum -= samples[i - block] * samples[i - block];
      if (sum > loudest) loudest = sum;
    }
  }
  return Math.sqrt(loudest / block);
}

/** Measures finished recordings: the loudest stretch of one, 0 to 1.
 * Rejects when the engine cannot decode the recording, or has not within
 * `deadlineMs`. An engine cannot be told to stop decoding, so a decode
 * given up on still holds its audio: until it settles, another recording
 * is refused rather than decoded beside it. */
export function recordingMeasurer(
  decode: (recording: Blob) => Promise<DecodedAudio> = decodeRecording,
  deadlineMs = LOUDNESS_DEADLINE_MS,
): (recording: Blob, stretchSeconds: number) => Promise<number> {
  let decoding = false;
  return async (recording, stretchSeconds) => {
    if (decoding) throw new Error('Another recording is still being measured.');
    decoding = true;
    const decoded = Promise.resolve().then(() => decode(recording));
    const settled = () => { decoding = false; };
    decoded.then(settled, settled);
    let timer: ReturnType<typeof setTimeout> | undefined;
    const late = new Promise<never>((_, reject) => {
      timer = globalThis.setTimeout(() => reject(new Error('The recording took too long to measure.')), deadlineMs);
    });
    try {
      return loudestStretch(await Promise.race([decoded, late]), stretchSeconds);
    } finally {
      globalThis.clearTimeout(timer);
    }
  };
}

export function recordingFormat(recorder: typeof MediaRecorder | undefined): RecordingFormat | null {
  if (recorder === undefined) return null;
  if (recorder.isTypeSupported('audio/webm;codecs=opus')) {
    return { container: 'audio/webm', recorderType: 'audio/webm;codecs=opus', timeslice: WEBM_TIMESLICE_MS };
  }
  if (recorder.isTypeSupported('audio/mp4')) {
    return { container: 'audio/mp4', recorderType: 'audio/mp4', timeslice: undefined };
  }
  return null;
}

export const browserDictationEnvironment: DictationEnvironment = {
  format: () => recordingFormat(globalThis.MediaRecorder),
  // Android stops asking after a second refusal; only its settings can
  // undo that.
  blockedMessage: () => isNativeShell()
    ? 'Microphone access is blocked. Allow it for Agent Overflow in Android settings.'
    : 'Microphone access is blocked. Allow it in the browser or system settings.',
  quietSpeech: pauseReadAloud,
  openMicrophone: () => {
    const media = globalThis.navigator?.mediaDevices;
    if (!media?.getUserMedia) {
      return Promise.reject(Object.assign(new Error('Recording needs a secure connection to this computer.'), { name: 'NotSupportedError' }));
    }
    return media.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  },
  createRecorder: (stream, format) =>
    new MediaRecorder(stream, { mimeType: format.recorderType, audioBitsPerSecond: AUDIO_BITS_PER_SECOND }) as unknown as RecorderLike,
  createLevelMeter,
  // AudioWorklet exists only in a secure context, as the microphone does.
  streamsPcm: () => globalThis.AudioContext !== undefined
    && globalThis.AudioWorkletNode !== undefined
    && 'audioWorklet' in globalThis.AudioContext.prototype,
  openPcm,
  recordingLoudness: recordingMeasurer(),
  silenceUnjudged: (err) => {
    reportFrontendDiagnostic('dictation: a recording the meter heard nothing in could not be measured', err instanceof Error ? err.message : '');
  },
  now: () => globalThis.performance.now(),
  setInterval: (fn, ms) => globalThis.setInterval(fn, ms),
  clearInterval: (handle) => globalThis.clearInterval(handle as ReturnType<typeof setInterval>),
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (handle) => globalThis.clearTimeout(handle as ReturnType<typeof setTimeout>),
};

/** RMS loudness of the microphone, read on demand. Null where the engine
 * has no Web Audio, which only disables the silence check and the ring. */
function createLevelMeter(stream: MediaStream): LevelMeter | null {
  const Context = globalThis.AudioContext;
  if (Context === undefined) return null;
  const context = new Context();
  let analyser: AnalyserNode;
  let source: MediaStreamAudioSourceNode;
  try {
    analyser = context.createAnalyser();
    analyser.fftSize = 1024;
    source = context.createMediaStreamSource(stream);
    source.connect(analyser);
  } catch (err) {
    void context.close().catch(() => {});
    throw err;
  }
  const samples = new Float32Array(analyser.fftSize);
  return {
    read() {
      analyser.getFloatTimeDomainData(samples);
      let sum = 0;
      for (const sample of samples) sum += sample * sample;
      return Math.sqrt(sum / samples.length);
    },
    stretchSeconds: samples.length / context.sampleRate,
    close() {
      source.disconnect();
      void context.close().catch(() => {});
    },
  };
}

/** 24 kHz 16-bit PCM from the microphone, captured on the audio thread
 * (pcmCapture.worklet.ts). The node feeds a muted gain into the
 * destination because an engine renders only what reaches it. */
async function openPcm(stream: MediaStream): Promise<PcmStream> {
  const context = new AudioContext();
  try {
    await context.audioWorklet.addModule(pcmCaptureUrl);
    const source = context.createMediaStreamSource(stream);
    const node = new AudioWorkletNode(context, PCM_CAPTURE_PROCESSOR, {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      channelCount: 1,
      channelCountMode: 'explicit',
    });
    const mute = context.createGain();
    mute.gain.value = 0;
    source.connect(node).connect(mute).connect(context.destination);
    let flushed: (() => void) | null = null;
    const pcm: PcmStream = {
      onPiece: null,
      flush: () => new Promise<void>((resolve, reject) => {
        const timer = globalThis.setTimeout(() => {
          flushed = null;
          reject(new Error('The microphone capture did not stop in time.'));
        }, FLUSH_TIMEOUT_MS);
        flushed = () => {
          globalThis.clearTimeout(timer);
          flushed = null;
          resolve();
        };
        node.port.postMessage('flush');
      }),
      close: () => {
        node.port.onmessage = null;
        node.port.close();
        source.disconnect();
        node.disconnect();
        mute.disconnect();
        void context.close().catch(() => {});
      },
    };
    node.port.onmessage = (event: MessageEvent) => {
      if (event.data === 'flushed') flushed?.();
      else if (event.data instanceof ArrayBuffer) pcm.onPiece?.(new Uint8Array(event.data));
    };
    // A context made after the microphone prompt may start suspended.
    await context.resume();
    return pcm;
  } catch (err) {
    void context.close().catch(() => {});
    throw err;
  }
}
