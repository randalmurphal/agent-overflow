// The loudness meter in real Chromium, on a real stream: the level shows
// the newest moment, while the silence check still hears a short sound
// that ended before the meter was read.

import { expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
import { browserDictationEnvironment } from './browserDictation';
import { SILENCE_PEAK } from './dictationSession.svelte';

/** Polls until `ready` holds, failing after `ms`. */
async function until(ready: () => boolean, what: string, ms = 5000): Promise<void> {
  const deadline = performance.now() + ms;
  while (!ready()) {
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

it('still hears a short sound after the level has gone quiet', async () => {
  // An AudioContext runs only after a gesture.
  await userEvent.click(document.body);
  const source = new AudioContext();
  const output = source.createMediaStreamDestination();
  const tone = new OscillatorNode(source, { frequency: 440 });
  const gate = new GainNode(source, { gain: 0 });
  tone.connect(gate).connect(output);
  tone.start();
  const meter = browserDictationEnvironment.createLevelMeter(output.stream);
  expect(meter, 'Chromium has Web Audio').not.toBeNull();
  try {
    await source.resume();
    expect(meter!.loudest(), 'a silent stream is silent').toBeLessThan(SILENCE_PEAK);

    // 40 ms of tone, then silence again.
    const at = source.currentTime + 0.05;
    gate.gain.setValueAtTime(0.5, at);
    gate.gain.setValueAtTime(0, at + 0.04);

    await until(() => meter!.loudest() >= SILENCE_PEAK, 'the tone to reach the meter');
    await until(() => meter!.read() < SILENCE_PEAK, 'the level to go quiet again');
    expect(meter!.loudest(), 'the tone is still held once the level is quiet').toBeGreaterThanOrEqual(SILENCE_PEAK);
  } finally {
    meter?.close();
    await source.close();
  }
});
