import { describe, expect, it } from 'vitest';
import { transportBannerReason, type TransportBannerInput } from './transportBannerReason';

function input(overrides: Partial<TransportBannerInput>): TransportBannerInput {
  return {
    paired: false,
    status: 'connected',
    checking: false,
    resyncing: false,
    unreachableSettled: false,
    hasEverConnected: true,
    bootGraceExpired: true,
    notice: false,
    ...overrides,
  };
}

describe('transportBannerReason', () => {
  // Every reason a paired computer's strip could show waits for the settle
  // time: a resume check and a resync happen while connected, so they never
  // show, and a drop or a refusal shows only once settled.
  it('shows nothing for a paired computer before the settle time, for any reason', () => {
    const paired = { paired: true };
    expect(transportBannerReason(input({ ...paired, checking: true }))).toBeNull();
    expect(transportBannerReason(input({ ...paired, resyncing: true }))).toBeNull();
    for (const status of ['reconnecting', 'unauthorized', 'pairing-required', 'starting', 'disconnected'] as const) {
      expect(transportBannerReason(input({ ...paired, status })), status).toBeNull();
      expect(transportBannerReason(input({ ...paired, status, notice: true })), status).toBeNull();
    }
    expect(transportBannerReason(input({ ...paired, status: 'reconnecting', unreachableSettled: true }))).toBe('outage');
    expect(transportBannerReason(input({ ...paired, status: 'unauthorized', unreachableSettled: true }))).toBe('outage');
    expect(transportBannerReason(input({ ...paired, notice: true }))).toBe('notice');
  });

  it("shows this computer's problems at once, as before", () => {
    expect(transportBannerReason(input({ checking: true }))).toBe('checking');
    expect(transportBannerReason(input({ resyncing: true }))).toBe('syncing');
    expect(transportBannerReason(input({ notice: true }))).toBe('notice');
    expect(transportBannerReason(input({}))).toBeNull();
    expect(transportBannerReason(input({ status: 'reconnecting' }))).toBe('outage');
    // A first connect waits out its grace; a start shows only as a restart.
    expect(transportBannerReason(input({ status: 'disconnected', hasEverConnected: false, bootGraceExpired: false }))).toBeNull();
    expect(transportBannerReason(input({ status: 'disconnected', hasEverConnected: false }))).toBe('outage');
    expect(transportBannerReason(input({ status: 'starting', hasEverConnected: false }))).toBeNull();
    expect(transportBannerReason(input({ status: 'starting' }))).toBe('outage');
  });
});
