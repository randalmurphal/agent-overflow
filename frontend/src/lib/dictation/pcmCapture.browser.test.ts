// The capture worklet in real Chromium, rendered offline so the input is
// exact: whole pieces post while rendering, and "flush" posts the
// part-filled tail before "flushed", so stopping loses no trailing speech.

import { expect, it } from 'vitest';
import pcmCaptureUrl from './pcmCapture.worklet.ts?worker&url';
import { PCM_CAPTURE_PROCESSOR, PIECE_SAMPLES } from './pcmDownsampler';

it('posts whole pieces while rendering and the part-filled tail at flush', async () => {
  // 100 render quanta at 48 kHz: 6400 samples at 24 kHz, two whole pieces
  // and a 1600-sample tail.
  const frames = 128 * 100;
  const context = new OfflineAudioContext(1, frames, 48_000);
  await context.audioWorklet.addModule(pcmCaptureUrl);
  const source = new ConstantSourceNode(context, { offset: 0.5 });
  const node = new AudioWorkletNode(context, PCM_CAPTURE_PROCESSOR, {
    numberOfInputs: 1,
    numberOfOutputs: 1,
    channelCount: 1,
    channelCountMode: 'explicit',
  });
  source.connect(node).connect(context.destination);
  const received: Array<Int16Array | 'flushed'> = [];
  const flushed = new Promise<void>((resolve) => {
    node.port.onmessage = (event: MessageEvent) => {
      if (event.data === 'flushed') {
        received.push('flushed');
        resolve();
      } else {
        received.push(new Int16Array(event.data as ArrayBuffer));
      }
    };
  });
  source.start();
  await context.startRendering();
  node.port.postMessage('flush');
  await flushed;

  expect(received.map((piece) => (piece === 'flushed' ? piece : piece.length))).toEqual([
    PIECE_SAMPLES, PIECE_SAMPLES, 1600, 'flushed',
  ]);
  const tail = received[2] as Int16Array;
  expect(tail.every((sample) => sample === Math.round(0.5 * 0x7fff))).toBe(true);
});
