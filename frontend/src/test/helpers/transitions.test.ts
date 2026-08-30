// The helper only earns its adoptions if it actually catches the leaks
// it claims to, and names the lap that leaked. Each failing case here is
// a subject that breaks exactly one lap.
import { describe, expect, it } from 'vitest';
import { expectCleanTransitions } from './transitions';

/** A correct door: one entry per engagement, released once. */
function makeRegistry() {
  const live = new Set<object>();
  return {
    live,
    on(): () => void {
      const entry = {};
      live.add(entry);
      let released = false;
      return () => {
        if (released) return;
        released = true;
        live.delete(entry);
      };
    },
  };
}

describe('expectCleanTransitions', () => {
  it('passes a subject that returns to rest on every lap', () => {
    const registry = makeRegistry();
    let inFlight = 0;
    expect(() => expectCleanTransitions('clean', {
      on: () => registry.on(),
      off: (handle) => { (handle as () => void)(); inFlight = 0; },
      whileOn: () => { expect(registry.live.size).toBe(1); },
      onAgain: () => { expect(registry.live.size).toBe(1); },
      inFlight: () => { inFlight++; },
      read: () => ({ live: registry.live.size, inFlight }),
    })).not.toThrow();
    expect(registry.live.size).toBe(0);
  });

  it('catches a leak on the first lap', () => {
    const live: object[] = [];
    expect(() => expectCleanTransitions('never releases', {
      on: () => { live.push({}); },
      off: () => {},
      read: () => ({ live: live.length }),
    })).toThrow(/"on -> off" did not return to the resting state/);
  });

  it('catches a teardown that is not idempotent', () => {
    let depth = 0;
    expect(() => expectCleanTransitions('double release', {
      on: () => { depth++; },
      off: () => { depth--; },
      read: () => ({ depth }),
    })).toThrow(/"on -> off -> off" did not return to the resting state/);
  });

  it('catches a second engagement the release cannot reach', () => {
    // The sink-registration shape: `on` adds an entry, but the release
    // only ever reaches the one it captured, so a double engagement
    // strands the other.
    const registry = makeRegistry();
    expect(() => expectCleanTransitions('double engage', {
      on: () => registry.on(),
      off: (handle) => { (handle as () => void)(); },
      onAgain: () => { registry.on(); },
      read: () => ({ live: registry.live.size }),
    })).toThrow(/"on -> on -> off" did not return to the resting state/);
  });

  it('catches state stranded by a teardown that lands mid-flight', () => {
    const registry = makeRegistry();
    let pending = 0;
    expect(() => expectCleanTransitions('stranded in flight', {
      on: () => registry.on(),
      // Releases the registration but abandons whatever was queued.
      off: (handle) => { (handle as () => void)(); },
      inFlight: () => { pending++; },
      read: () => ({ live: registry.live.size, pending }),
    })).toThrow(/"on -> in flight -> off" did not return to the resting state/);
  });

  it('runs the optional laps only when the subject describes them', () => {
    const laps: string[] = [];
    expectCleanTransitions('minimal', {
      on: () => { laps.push('on'); },
      off: () => { laps.push('off'); },
      read: () => ({}),
    });
    // Four engagements: the three mandatory laps, one of which cycles twice.
    expect(laps.filter((lap) => lap === 'on')).toHaveLength(4);
  });
});
