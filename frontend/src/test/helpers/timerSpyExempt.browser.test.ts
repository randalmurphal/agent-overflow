import { describe, expect, it } from 'vitest';
import { spyScheduling } from './schedulingSpy';

const pause = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
const armed = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe('spyScheduling', () => {
  // V8 names a function called as a property of an object by its receiver
  // too ("Object.pause"), which is how a scan calls the yield it is handed.
  it('exempts a named function called directly or as a property of an options object, and records the others', async () => {
    const spy = spyScheduling('timerSpyExempt.browser.test', ['pause']);
    try {
      await pause();
      await { pause }.pause();
      expect(spy.calls).toEqual([]);
      await armed();
      await { armed }.armed();
      expect(spy.calls).toHaveLength(2);
      expect(spy.calls.every((call) => call.startsWith('timeout at '))).toBe(true);
    } finally {
      spy.restore();
    }
  });
});
