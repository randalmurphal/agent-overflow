import { describe, expect, it } from 'vitest';
import {
  observedScrollTopFromEvent,
  recordScrollEventObservation,
} from './eventObservation';

describe('scroll event observation', () => {
  it('records primitive event data without changing public event fields', () => {
    const event = new Event('scroll');

    recordScrollEventObservation(event, 42.5);

    expect(observedScrollTopFromEvent(event)).toBe(42.5);
    expect(Object.keys(event)).not.toContain('scrollTop');
  });

});
