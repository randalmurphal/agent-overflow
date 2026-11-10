// A finished recording measured in real Chromium: recorded in pieces by a
// real MediaRecorder, joined as the session joins them, and decoded by the
// engine. The outcome depends on what was recorded, not on when a timer ran.

import { expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
import { recordingFormat, recordingLoudness } from './browserDictation';
import { SILENCE_PEAK } from './dictationSession.svelte';

/** Records `stream` in 100 ms pieces until `context`'s clock passes
 * `until` seconds, and answers the pieces joined. */
async function record(context: AudioContext, stream: MediaStream, until: number): Promise<Blob> {
  const format = recordingFormat(MediaRecorder)!;
  const recorder = new MediaRecorder(stream, { mimeType: format.recorderType });
  const pieces: Blob[] = [];
  recorder.ondataavailable = ({ data }) => { if (data.size > 0) pieces.push(data); };
  const stopped = new Promise<void>((resolve) => { recorder.onstop = () => resolve(); });
  recorder.start(100);
  while (context.currentTime < until) await new Promise((resolve) => setTimeout(resolve, 20));
  recorder.stop();
  await stopped;
  return new Blob(pieces, { type: format.container });
}

/** The measured loudness of `length` seconds of silence carrying a tone
 * for each burst. */
async function measured(bursts: Array<{ at: number; seconds: number }>, length: number): Promise<number> {
  // An AudioContext runs only after a gesture.
  await userEvent.click(document.body);
  const context = new AudioContext();
  try {
    await context.resume();
    const output = context.createMediaStreamDestination();
    const source = new OscillatorNode(context, { frequency: 440 });
    const gate = new GainNode(context, { gain: 0 });
    source.connect(gate).connect(output);
    source.start();
    const start = context.currentTime + 0.1;
    for (const { at, seconds } of bursts) {
      gate.gain.setValueAtTime(0.5, start + at);
      gate.gain.setValueAtTime(0, start + at + seconds);
    }
    return await recordingLoudness(await record(context, output.stream, start + length));
  } finally {
    await context.close();
  }
}

it('measures a recording of silence as silent', async () => {
  expect(await measured([], 0.8)).toBeLessThan(SILENCE_PEAK);
});

it('hears one short sound in a recording that is otherwise silent', async () => {
  expect(await measured([{ at: 0.4, seconds: 0.04 }], 1)).toBeGreaterThanOrEqual(SILENCE_PEAK);
});

it('rejects what is not a recording', async () => {
  await expect(recordingLoudness(new Blob(['not audio'], { type: 'audio/webm' }))).rejects.toThrow();
});
