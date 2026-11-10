// The AudioWorklet half of live dictation's capture: runs on the audio
// thread, downsamples the microphone to 24 kHz 16-bit PCM and posts each
// 100 ms piece to the page, transferring its buffer. A "flush" message
// posts the part-filled piece and then "flushed", so stopping loses no
// trailing speech. Bundled as its own same-origin module
// (browserDictation.ts), which the page's CSP admits.

import { PCM_CAPTURE_PROCESSOR, PcmDownsampler } from './pcmDownsampler';

// The AudioWorkletGlobalScope names TypeScript's DOM library leaves out.
declare const sampleRate: number;
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
}
declare function registerProcessor(name: string, processor: new () => AudioWorkletProcessor): void;

class PcmCaptureProcessor extends AudioWorkletProcessor {
  private readonly downsampler: PcmDownsampler;

  constructor() {
    super();
    this.downsampler = new PcmDownsampler(sampleRate, (piece) => {
      this.port.postMessage(piece.buffer, [piece.buffer]);
    });
    this.port.onmessage = (event: MessageEvent) => {
      if (event.data !== 'flush') return;
      this.downsampler.flush();
      this.port.postMessage('flushed');
    };
  }

  process(inputs: Float32Array[][]): boolean {
    const channel = inputs[0]?.[0];
    if (channel) this.downsampler.push(channel);
    return true;
  }
}

registerProcessor(PCM_CAPTURE_PROCESSOR, PcmCaptureProcessor);
