import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  getProviderRateLimit,
  resetForTest,
  setProviderRateLimits,
} from './rateLimitsInfo.svelte';

describe('rateLimitsInfo', () => {
  beforeEach(() => {
    resetForTest();
  });
  afterEach(() => {
    resetForTest();
  });

  it('returns null for an unseeded provider/window combination', () => {
    expect(getProviderRateLimit('claude', 300)).toBeNull();
    expect(getProviderRateLimit('codex', 10080)).toBeNull();
  });

  it('returns null when provider is undefined', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    expect(getProviderRateLimit(undefined, 300)).toBeNull();
  });

  it('stores and retrieves a single-window snapshot', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    const got = getProviderRateLimit('claude', 300);
    expect(got).not.toBeNull();
    expect(got?.usedPercent).toBe(42);
    expect(got?.resetsAt).toBe(1776283200);
  });

  // Claude wires emit ONE window per `rate_limit_event` (5h XOR 7d).
  // The store must merge new windows alongside existing ones rather
  // than replacing — otherwise toggling between 5h and 7d events
  // alternately wipes the other window.
  it('merges Claude single-window updates across separate calls', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'seven_day', limitName: '7d', usedPercent: 51, windowMins: 10080, resetsAt: 1776981600 },
      ],
      updatedAt: 1776283500,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(30);
    expect(getProviderRateLimit('claude', 10080)?.usedPercent).toBe(51);
  });

  // Codex pushes both windows together — the same merge path handles it.
  it('stores Codex multi-window snapshots in one call', () => {
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'primary', limitName: '5h', usedPercent: 25, windowMins: 300, resetsAt: 1776283200 },
        { limitId: 'secondary', limitName: '7d', usedPercent: 60, windowMins: 10080, resetsAt: 1776981600 },
      ],
      updatedAt: 1776283000,
    });

    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(25);
    expect(getProviderRateLimit('codex', 10080)?.usedPercent).toBe(60);
  });

  // A subsequent snapshot for the SAME window should overwrite that
  // slot (latest reading wins) without disturbing the other window.
  it('overwrites the same-window slot on a fresh update without disturbing the other window', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776283200 },
        { limitId: 'seven_day', limitName: '7d', usedPercent: 51, windowMins: 10080, resetsAt: 1776981600 },
      ],
      updatedAt: 1776283000,
    });
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776284000,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(95);
    expect(getProviderRateLimit('claude', 10080)?.usedPercent).toBe(51);
  });

  // Provider isolation: snapshots in different provider slots must not
  // interact. A Codex update must not bleed into the Claude slot.
  it('isolates provider slots', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'primary', limitName: '5h', usedPercent: 88, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283500,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(30);
    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(88);
  });

  // windowMins=0 is the parser's signal for "unrecognised window length"
  // (Claude's `windowMinsForRateLimitType` fallback). The store must
  // drop those entries — the toolbar reads `(provider, 300)` and
  // `(provider, 10080)` only, so a stray 0-keyed slot would just leak
  // memory forever.
  it('filters out windowMins=0 entries to avoid unrenderable slots', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'thirty_day', limitName: 'thirty_day', usedPercent: 10, windowMins: 0, resetsAt: 1776283200 },
        { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });

    expect(getProviderRateLimit('claude', 0)).toBeNull();
    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);
  });

  // Empty snapshots are no-ops — the store is "last value wins until a
  // non-empty update comes." A defensive wipe on empty would
  // reintroduce the per-pane flicker bug at the global level.
  it('treats empty-limits snapshots as no-ops (does not wipe last-known values)', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    setProviderRateLimits({
      provider: 'claude',
      limits: [],
      updatedAt: 1776284000,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);
  });

  // The window-reset stale-event defense.
  //
  // Around a window-reset boundary, multiple Claude sessions
  // emit independently. A long-running session can keep emitting
  // its pre-reset reading for several requests after a fresher
  // session has observed the post-reset reading. Without a guard
  // these events overwrite each other and the ring visibly
  // oscillates between the old high percent and the new low one.
  //
  // `resetsAt` is the natural version stamp: a pre-reset event's
  // resetsAt is the boundary about to fire, a post-reset event's
  // resetsAt is the boundary 5h/7d later. Drop incoming entries
  // whose resetsAt is older than what's already stored.
  it('drops a stale pre-reset entry when a post-reset entry is already stored', () => {
    // Post-reset reading lands first.
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 5, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776284000,
    });

    // Stale pre-reset reading from a slow session arrives next —
    // its resetsAt is the OLD boundary that has already fired.
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: 1776282000 },
      ],
      updatedAt: 1776285000,
    });

    // Post-reset value sticks; the stale event was dropped.
    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(5);
    expect(getProviderRateLimit('claude', 300)?.resetsAt).toBe(1776300000);
  });

  // Window rollover from below: existing (pre-reset) is replaced by
  // incoming (post-reset). This is the path the global store
  // observes the boundary itself; the next test covers what happens
  // afterwards when a stale session keeps emitting.
  it('takes a post-reset entry when the existing entry is from before the boundary', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: 1776282000 },
      ],
      updatedAt: 1776280000,
    });
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 5, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776284000,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(5);
    expect(getProviderRateLimit('claude', 300)?.resetsAt).toBe(1776300000);
  });

  // Equal `resetsAt` = same window, latest reading wins. Within a
  // window, usedPercent climbs monotonically as the user makes
  // requests. Each new event carries the most current reading and
  // must update — the stale-event defense is keyed strictly on a
  // newer boundary, not on equal boundaries.
  it('updates on a same-window-later-reading event (equal resetsAt is not stale)', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 30, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776284000,
    });
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776285000,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(42);
  });

  // The defense applies per-window: a stale 5h event must not
  // disturb a fresh 7d entry, and vice versa. Otherwise a snapshot
  // carrying both windows where one is stale would taint the other.
  it('applies stale-event defense per window slot', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 5, windowMins: 300, resetsAt: 1776300000 },
        { limitId: 'seven_day', limitName: '7d', usedPercent: 51, windowMins: 10080, resetsAt: 1776981600 },
      ],
      updatedAt: 1776284000,
    });

    // Stale 5h, fresh 7d in the same snapshot.
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: 1776282000 },
        { limitId: 'seven_day', limitName: '7d', usedPercent: 60, windowMins: 10080, resetsAt: 1776981600 },
      ],
      updatedAt: 1776285000,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(5);
    expect(getProviderRateLimit('claude', 10080)?.usedPercent).toBe(60);
  });

  // Unknown providers (anything other than claude/codex) are dropped
  // rather than coerced. This keeps the store's key space tight to
  // the union type RateLimitMeter consumes.
  it('drops snapshots for unrecognised providers', () => {
    setProviderRateLimits({
      // The Go type allows any string, the TS type narrows; defense
      // in depth — dropping unknown providers means no untyped slots
      // leak into the Map.
      provider: 'mistral' as unknown as 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 99, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    expect(getProviderRateLimit('claude', 300)).toBeNull();
    expect(getProviderRateLimit('codex', 300)).toBeNull();
  });
});
