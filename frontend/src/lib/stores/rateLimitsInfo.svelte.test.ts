import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  clearProviderRateLimits,
  getProviderRateLimit,
  getProviderRateLimits,
  getProviderRateLimitsForWindow,
  rateLimitDisplayName,
  resetForTest,
  setProviderRateLimits,
} from './rateLimitsInfo.svelte';

describe('rateLimitsInfo', () => {
  beforeEach(() => {
    resetForTest();
    // Getters project entries whose reset boundary has passed to 0% used,
    // so the fixture epochs below (Apr 2026) must sit in the future.
    vi.useFakeTimers();
    vi.setSystemTime(new Date(1_776_000_000_000));
  });
  afterEach(() => {
    resetForTest();
    vi.useRealTimers();
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

  it('does not fall back to another account when an explicit account is unseeded', () => {
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'codex', limitName: '5h', usedPercent: 64, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });

    expect(getProviderRateLimit('codex', 300, 'different-account')).toBeNull();
    expect(
      getProviderRateLimitsForWindow('codex', 300, 'different-account'),
    ).toEqual({ primary: null, limits: [] });
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
        { limitId: 'codex', limitName: '5h', usedPercent: 25, windowMins: 300, resetsAt: 1776283200 },
        { limitId: 'codex', limitName: '7d', usedPercent: 60, windowMins: 10080, resetsAt: 1776981600 },
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
        { limitId: 'codex', limitName: '5h', usedPercent: 88, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283500,
    });

    expect(getProviderRateLimit('claude', 300)?.usedPercent).toBe(30);
    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(88);
  });

  // A new provider bucket can arrive before its duration is known locally.
  // Keep it for the dynamic Settings list without exposing it as a toolbar
  // duration slot.
  it('retains windowMins=0 entries for settings but not toolbar lookups', () => {
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
    expect(getProviderRateLimits('claude').map((entry) => entry.limitId)).toContain('thirty_day');
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

  // Equal `resetsAt` = same window. Usage climbs monotonically within
  // a window, so a higher same-window reading is the fresher value.
  it('updates on a same-window higher-reading event', () => {
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

  it('accepts higher usage when the provider jitters a reset boundary by seconds', () => {
    setProviderRateLimits({
      provider: 'claude',
      accountId: 'active',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 0, windowMins: 300, resetsAt: 1784841601 },
      ],
      updatedAt: 1784823000,
    });
    setProviderRateLimits({
      provider: 'claude',
      accountId: 'active',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 8, windowMins: 300, resetsAt: 1784841599 },
      ],
      updatedAt: 1784827200,
    });

    const limit = getProviderRateLimits('claude', 'active')[0];
    expect(limit?.usedPercent).toBe(8);
    expect(limit?.resetsAt).toBe(1784841601);
  });

  it('does not churn or regress usage for reset timestamp jitter alone', () => {
    setProviderRateLimits({
      provider: 'claude',
      accountId: 'equal',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 8, windowMins: 300, resetsAt: 1784841601 },
      ],
      updatedAt: 1784823000,
    });
    setProviderRateLimits({
      provider: 'claude',
      accountId: 'lower',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 8, windowMins: 300, resetsAt: 1784841601 },
      ],
      updatedAt: 1784823000,
    });
    const equalBefore = getProviderRateLimits('claude', 'equal')[0];
    const lowerBefore = getProviderRateLimits('claude', 'lower')[0];

    setProviderRateLimits({
      provider: 'claude',
      accountId: 'equal',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 8, windowMins: 300, resetsAt: 1784841599 },
      ],
      updatedAt: 1784827200,
    });
    setProviderRateLimits({
      provider: 'claude',
      accountId: 'lower',
      limits: [
        { limitId: 'session', limitName: 'Current session', usedPercent: 7, windowMins: 300, resetsAt: 1784841599 },
      ],
      updatedAt: 1784827200,
    });

    expect(getProviderRateLimits('claude', 'equal')[0]).toBe(equalBefore);
    expect(getProviderRateLimits('claude', 'lower')[0]).toBe(lowerBefore);
  });

  // Delayed probe/notification races can replay an older lower reading
  // for the same reset boundary after a fresher one. Keep the higher
  // value until the reset boundary advances.
  it('drops a same-window lower-reading event', () => {
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'codex', limitName: 'Codex', usedPercent: 39, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776285000,
    });
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'codex', limitName: 'Codex', usedPercent: 9, windowMins: 300, resetsAt: 1776300000 },
      ],
      updatedAt: 1776286000,
    });

    expect(getProviderRateLimit('codex', 300)?.usedPercent).toBe(39);
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

  it('isolates two accounts on the same provider', () => {
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'first',
      limits: [
        { limitId: 'codex', limitName: 'Codex', usedPercent: 91, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'second',
      limits: [
        { limitId: 'codex', limitName: 'Codex', usedPercent: 12, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });

    expect(getProviderRateLimits('codex', 'first')[0]?.usedPercent).toBe(91);
    expect(getProviderRateLimits('codex', 'second')[0]?.usedPercent).toBe(12);
  });

  it('clears only the removed account limits', () => {
    for (const [accountId, usedPercent] of [['first', 91], ['second', 12]] as const) {
      setProviderRateLimits({
        provider: 'codex',
        accountId,
        limits: [
          {
            limitId: 'codex',
            limitName: 'Codex',
            usedPercent,
            windowMins: 300,
            resetsAt: 1776283200,
          },
        ],
        updatedAt: 1776283000,
      });
    }

    clearProviderRateLimits('codex', 'first');

    expect(getProviderRateLimits('codex', 'first')).toEqual([]);
    expect(getProviderRateLimit('codex', 300, 'second')?.usedPercent).toBe(12);
  });

  it('retains multiple dynamic buckets for the same window', () => {
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'one',
      limits: [
        { limitId: 'codex', limitName: 'Codex', usedPercent: 100, windowMins: 300, resetsAt: 1776283200 },
        { limitId: 'spark', limitName: 'GPT-5.3-Codex-Spark', usedPercent: 46, windowMins: 300, resetsAt: 1776283200 },
      ],
      updatedAt: 1776283000,
    });

    const limits = getProviderRateLimits('codex', 'one');
    expect(limits.map((entry) => entry.limitId)).toEqual(['codex', 'spark']);
    expect(limits.map((entry) => entry.usedPercent)).toEqual([100, 46]);
  });

  it('groups composer details by duration with the provider default first', () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'weekly_scoped:fable', limitName: 'Fable', usedPercent: 99, windowMins: 10080, resetsAt: 1776981600 },
        { limitId: 'weekly_all', limitName: 'All models', usedPercent: 52, windowMins: 10080, resetsAt: 1776981600 },
        { limitId: 'monthly_other', limitName: 'Monthly other', usedPercent: 80, windowMins: 43200, resetsAt: 1779000000 },
      ],
      updatedAt: 1776283000,
    });

    const group = getProviderRateLimitsForWindow('claude', 10080);
    expect(group.primary?.limitId).toBe('weekly_all');
    expect(group.limits.map((entry) => entry.limitId)).toEqual([
      'weekly_all',
      'weekly_scoped:fable',
    ]);
    expect(group.limits.map((entry) => entry.usedPercent)).toEqual([52, 99]);
  });

  it('keeps scoped-only composer details separate from the account-wide primary', () => {
    setProviderRateLimits({
      provider: 'codex',
      limits: [
        { limitId: 'spark', limitName: 'GPT-5.3-Codex-Spark', usedPercent: 99, windowMins: 300, resetsAt: 1776981600 },
      ],
      updatedAt: 1776283000,
    });

    const group = getProviderRateLimitsForWindow('codex', 300);
    expect(group.primary).toBeNull();
    expect(group.limits.map((entry) => entry.limitId)).toEqual(['spark']);
    expect(getProviderRateLimit('codex', 300)).toBeNull();
  });

  // Reset-boundary expiry. The stored entry is never rewritten when its
  // boundary passes: getters project it to 0% used at read time and keep
  // the server-reported boundary. Fabricating a `old + window` boundary
  // instead is what broke the 5h ring on 2026-08-07 — Claude re-anchors
  // 5h windows, so the real next boundary landed 10 minutes EARLIER than
  // the fabricated one and the stale-window guard rejected every real
  // post-reset snapshot for the entire window.
  describe('reset boundary expiry', () => {
    // Real numbers from the incident: previous window closed 23:40:00Z,
    // the next window closed 04:30:00Z — 4h50m later, not 5h.
    const OLD_BOUNDARY = 1786146000;
    const NEXT_BOUNDARY = 1786163400;

    beforeEach(() => {
      vi.setSystemTime(new Date((OLD_BOUNDARY - 3600) * 1000));
      setProviderRateLimits({
        provider: 'claude',
        accountId: 'acct',
        limits: [
          { limitId: 'session', limitName: 'Current session', usedPercent: 42, windowMins: 300, resetsAt: OLD_BOUNDARY },
        ],
        updatedAt: (OLD_BOUNDARY - 3600) * 1000,
      });
    });

    it('shows 0% once the boundary passes, keeping the stored boundary', () => {
      vi.setSystemTime(new Date((OLD_BOUNDARY + 30) * 1000));
      const limit = getProviderRateLimit('claude', 300, 'acct');
      expect(limit?.usedPercent).toBe(0);
      expect(limit?.resetsAt).toBe(OLD_BOUNDARY);
    });

    it('fires the expiry timer at the boundary without rewriting entries', () => {
      vi.advanceTimersByTime(3601 * 1000);
      const limit = getProviderRateLimit('claude', 300, 'acct');
      expect(limit?.usedPercent).toBe(0);
      expect(limit?.resetsAt).toBe(OLD_BOUNDARY);
    });

    it('accepts the re-anchored next window whose boundary lands earlier than old + window', () => {
      vi.setSystemTime(new Date((OLD_BOUNDARY + 300) * 1000));
      setProviderRateLimits({
        provider: 'claude',
        accountId: 'acct',
        limits: [
          { limitId: 'session', limitName: 'Current session', usedPercent: 18, windowMins: 300, resetsAt: NEXT_BOUNDARY },
        ],
        updatedAt: (OLD_BOUNDARY + 300) * 1000,
      });
      const limit = getProviderRateLimit('claude', 300, 'acct');
      expect(limit?.usedPercent).toBe(18);
      expect(limit?.resetsAt).toBe(NEXT_BOUNDARY);
    });

    it('keeps showing 0% when a stale pre-reset reading lands after the boundary', () => {
      vi.setSystemTime(new Date((OLD_BOUNDARY + 300) * 1000));
      setProviderRateLimits({
        provider: 'claude',
        accountId: 'acct',
        limits: [
          { limitId: 'session', limitName: 'Current session', usedPercent: 43, windowMins: 300, resetsAt: OLD_BOUNDARY },
        ],
        updatedAt: (OLD_BOUNDARY + 300) * 1000,
      });
      expect(getProviderRateLimit('claude', 300, 'acct')?.usedPercent).toBe(0);
    });

    it('projects weekly windows the same way', () => {
      const weeklyBoundary = OLD_BOUNDARY + 24 * 3600;
      setProviderRateLimits({
        provider: 'claude',
        accountId: 'acct',
        limits: [
          { limitId: 'weekly_all', limitName: 'All models', usedPercent: 51, windowMins: 10080, resetsAt: weeklyBoundary },
        ],
        updatedAt: (OLD_BOUNDARY - 3600) * 1000,
      });
      vi.setSystemTime(new Date((weeklyBoundary + 30) * 1000));
      const limit = getProviderRateLimit('claude', 10080, 'acct');
      expect(limit?.usedPercent).toBe(0);
      expect(limit?.resetsAt).toBe(weeklyBoundary);
    });

    it('shows 0% for a snapshot hydrated with an already-passed boundary', () => {
      // App reopened after sitting idle past the boundary: the backend's
      // retained snapshot still carries pre-reset usage. No timer ever
      // fires for it (the boundary is already in the past at store time),
      // so only the read-time projection can honor the reset.
      vi.setSystemTime(new Date((OLD_BOUNDARY + 7200) * 1000));
      setProviderRateLimits({
        provider: 'claude',
        accountId: 'idle-acct',
        limits: [
          { limitId: 'session', limitName: 'Current session', usedPercent: 87, windowMins: 300, resetsAt: OLD_BOUNDARY },
        ],
        updatedAt: (OLD_BOUNDARY - 3600) * 1000,
      });
      expect(getProviderRateLimit('claude', 300, 'idle-acct')?.usedPercent).toBe(0);
    });
  });

  it('provides readable names for unnamed provider limits', () => {
    expect(rateLimitDisplayName({
      limitId: 'codex',
      limitName: '',
    })).toBe('All models');
    expect(rateLimitDisplayName({
      limitId: 'weekly_scoped:fable',
      limitName: '',
    })).toBe('Fable');
  });
});
