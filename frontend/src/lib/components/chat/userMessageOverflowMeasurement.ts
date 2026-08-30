import { flushSync } from 'svelte';
import { USER_MESSAGE_CLAMP_EPSILON_PX } from './userMessageClamp';

export interface UserMessageOverflowProbe {
  element(): HTMLElement | undefined;
  active(): boolean;
  apply(overflows: boolean): void;
}

export interface UserMessageOverflowCoordinator {
  register(probe: UserMessageOverflowProbe): () => void;
  request(probe: UserMessageOverflowProbe): void;
  requestAll(): void;
  measureAll(): void;
}

export const USER_MESSAGE_OVERFLOW_COORDINATOR_CONTEXT = Symbol(
  'user-message-overflow-coordinator',
);

const pending = new Set<UserMessageOverflowProbe>();
let pendingFlush = false;

function measure(probes: Iterable<UserMessageOverflowProbe>): void {
  const results: Array<{ probe: UserMessageOverflowProbe; overflows: boolean }> = [];
  const errors: unknown[] = [];
  for (const probe of probes) {
    pending.delete(probe);
    try {
      const element = probe.element();
      if (!element?.isConnected || !probe.active()) continue;
      results.push({
        probe,
        overflows:
          element.scrollHeight - element.clientHeight > USER_MESSAGE_CLAMP_EPSILON_PX,
      });
    } catch (error) {
      errors.push(error);
    }
  }

  // Read every paragraph before writing any state. The first geometry read
  // may flush layout after a row mount or text replacement; the rest of the
  // batch then reuse that layout instead of alternating layout and button
  // insertion once per mounted user message.
  if (results.length > 0) {
    try {
      flushSync(() => {
        for (const { probe, overflows } of results) {
          try {
            probe.apply(overflows);
          } catch (error) {
            errors.push(error);
          }
        }
      });
    } catch (error) {
      errors.push(error);
    }
  }
  if (errors.length === 1) throw errors[0];
  if (errors.length > 1) {
    throw new AggregateError(errors, 'user message overflow measurement failed');
  }
}

function schedule(probe: UserMessageOverflowProbe): void {
  pending.add(probe);
  schedulePendingFlush();
}

function schedulePendingFlush(): void {
  if (pendingFlush) return;
  pendingFlush = true;
  queueMicrotask(() => {
    pendingFlush = false;
    const batch = Array.from(pending);
    measure(batch);
  });
}

export function createUserMessageOverflowCoordinator(): UserMessageOverflowCoordinator {
  const probes = new Set<UserMessageOverflowProbe>();
  return {
    register(probe) {
      if (probes.has(probe)) {
        throw new Error('user message overflow probe is already registered');
      }
      probes.add(probe);
      let registered = true;
      return () => {
        if (!registered) return;
        registered = false;
        probes.delete(probe);
        pending.delete(probe);
      };
    },
    request(probe) {
      if (probes.has(probe)) schedule(probe);
    },
    requestAll() {
      for (const probe of probes) pending.add(probe);
      if (probes.size > 0) schedulePendingFlush();
    },
    measureAll() {
      measure(probes);
    },
  };
}

export function requestUserMessageOverflowMeasurement(
  probe: UserMessageOverflowProbe,
): void {
  schedule(probe);
}

export function measureUserMessageOverflowNow(probe: UserMessageOverflowProbe): void {
  measure([probe]);
}
