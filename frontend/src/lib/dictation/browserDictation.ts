// The browser half of a DictationSession: microphone, MediaRecorder and a
// loudness meter, as the engine provides them.

import { isNativeShell } from '../native/platform';
import type { DictationEnvironment, LevelMeter, RecorderLike, RecordingFormat } from './dictationSession.svelte';

/** Speech needs little: 32 kbps Opus is about 2.4 MB for ten minutes. */
const AUDIO_BITS_PER_SECOND = 32_000;

/** Chromium and Firefox stream WebM pieces this often while recording. */
const WEBM_TIMESLICE_MS = 1000;

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
  const analyser = context.createAnalyser();
  analyser.fftSize = 1024;
  const source = context.createMediaStreamSource(stream);
  source.connect(analyser);
  const samples = new Float32Array(analyser.fftSize);
  return {
    read() {
      analyser.getFloatTimeDomainData(samples);
      let sum = 0;
      for (const sample of samples) sum += sample * sample;
      return Math.sqrt(sum / samples.length);
    },
    close() {
      source.disconnect();
      void context.close().catch(() => {});
    },
  };
}
