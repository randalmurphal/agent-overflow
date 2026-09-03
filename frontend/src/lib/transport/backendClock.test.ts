// The backend-clock seam: a timestamp minted on one machine, read on
// another whose clock disagrees.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  backendClockSkew,
  backendNow,
  forgetBackendClock,
  registerBackendClock,
  resetBackendClocksForTest,
} from './backendClock';
import { HOME_BACKEND } from './backendKey';
import { relativeTime } from '../utils/format';

const NOW = 1_700_000_000_000;

describe('backendClock', () => {
  beforeEach(() => {
    resetBackendClocksForTest();
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });

  afterEach(() => {
    vi.useRealTimers();
    resetBackendClocksForTest();
  });

  it('answers this device’s own clock for a backend that never said', () => {
    expect(backendClockSkew('laptop')).toBe(0);
    expect(backendNow('laptop')).toBe(NOW);
    expect(backendNow()).toBe(NOW);
  });

  it('reads the source on every call, not once at registration', () => {
    // wsClient does not publish a hello whose only change is the clock,
    // so a copied number would be the skew from some earlier reconnect.
    let skew = 1_000;
    registerBackendClock('laptop', () => skew);
    expect(backendNow('laptop')).toBe(NOW + 1_000);
    skew = 60_000;
    expect(backendNow('laptop')).toBe(NOW + 60_000);
  });

  it('keeps each backend on its own clock', () => {
    registerBackendClock(HOME_BACKEND, () => 0);
    registerBackendClock('laptop', () => 5 * 60_000);
    expect(backendNow()).toBe(NOW);
    expect(backendNow('laptop')).toBe(NOW + 5 * 60_000);
  });

  it('falls back to this device when a source answers nonsense', () => {
    registerBackendClock('laptop', () => Number.NaN);
    expect(backendNow('laptop')).toBe(NOW);
  });

  it('forgets a detached backend', () => {
    registerBackendClock('laptop', () => 90_000);
    forgetBackendClock('laptop');
    expect(backendNow('laptop')).toBe(NOW);
  });
});

describe('relativeTime against a skewed backend', () => {
  beforeEach(() => {
    resetBackendClocksForTest();
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });

  afterEach(() => {
    vi.useRealTimers();
    resetBackendClocksForTest();
  });

  it('reads a fresh row as fresh when the backend clock runs ahead', () => {
    // The backend stamped a row with its own "now", five minutes ahead of
    // this device. Compared against Date.now() the row is in the FUTURE,
    // which the guard reads as "just now" by luck rather than by rule;
    // one minute later it reads as four minutes in the future still, and
    // the row sits on "just now" for five minutes while it ages.
    registerBackendClock('laptop', () => 5 * 60_000);
    const stampedByBackend = NOW + 5 * 60_000;

    vi.setSystemTime(NOW + 3 * 60_000);
    expect(relativeTime(stampedByBackend, 'locale', 'laptop')).toBe('3m ago');
    // The uncorrected reading is the bug: three real minutes of history
    // still renders as if nothing had happened.
    expect(relativeTime(stampedByBackend, 'locale')).toBe('just now');
  });

  it('reads a fresh row as fresh when the backend clock runs behind', () => {
    registerBackendClock('laptop', () => -30 * 60_000);
    const stampedByBackend = NOW - 30 * 60_000;

    expect(relativeTime(stampedByBackend, 'locale', 'laptop')).toBe('just now');
    expect(relativeTime(stampedByBackend, 'locale')).toBe('30m ago');
  });

  it('leaves the absolute formats on this device’s clock', () => {
    registerBackendClock('laptop', () => 5 * 60_000);
    const at = NOW;
    expect(relativeTime(at, '24-hour', 'laptop')).toBe(relativeTime(at, '24-hour'));
    expect(relativeTime(at, '12-hour', 'laptop')).toBe(relativeTime(at, '12-hour'));
  });
});
