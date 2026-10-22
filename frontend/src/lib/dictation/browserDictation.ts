// The browser half of a DictationSession: microphone, MediaRecorder, the
// live PCM capture and a loudness meter, as the engine provides them.

import { isNativeShell } from '../native/platform';
import { pauseReadAloud } from '../stores/readAloud.svelte';
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
