import { describe, expect, it } from 'vitest';
import type { Thread } from '../../types/models';
import { hasUnread, resolveThreadStatusPill } from './threadStatusPill';

type MinimalThread = Pick<Thread, 'mode' | 'lastReadAt' | 'latestTurnCompletedAt'>;

function t(overrides: Partial<MinimalThread> = {}): MinimalThread {
  return {
    mode: 'chat',
    lastReadAt: undefined,
    latestTurnCompletedAt: 1_000,
    ...overrides,
  };
}

describe('hasUnread', () => {
  it('returns false when lastReadAt is undefined (never tracked)', () => {
    expect(hasUnread(t({ lastReadAt: undefined, latestTurnCompletedAt: 2_000 }))).toBe(false);
  });

  it('returns false when there is no completed turn', () => {
    expect(hasUnread(t({ lastReadAt: 0, latestTurnCompletedAt: undefined }))).toBe(false);
  });

  it('returns false when the thread was read at or after its latest completed turn', () => {
    expect(hasUnread(t({ lastReadAt: 2_000, latestTurnCompletedAt: 2_000 }))).toBe(false);
    expect(hasUnread(t({ lastReadAt: 3_000, latestTurnCompletedAt: 2_000 }))).toBe(false);
  });

  it('returns true when activity postdates the last read', () => {
    expect(hasUnread(t({ lastReadAt: 1_000, latestTurnCompletedAt: 2_000 }))).toBe(true);
  });

  it('returns true for explicit unread marker at epoch 0', () => {
    expect(hasUnread(t({ lastReadAt: 0, latestTurnCompletedAt: 2_000 }))).toBe(true);
  });

  it('ignores metadata-only updatedAt changes', () => {
    expect(hasUnread(t({ lastReadAt: 1_000, latestTurnCompletedAt: 1_000 }))).toBe(false);
  });
});

describe('resolveThreadStatusPill', () => {
  it('returns null for idle + read (no pill at all)', () => {
    expect(
      resolveThreadStatusPill(t({ lastReadAt: 2_000, latestTurnCompletedAt: 1_000 }), 'idle'),
    ).toBeNull();
  });

  it('returns null for idle + never-tracked', () => {
    expect(
      resolveThreadStatusPill(t({ lastReadAt: undefined }), 'idle'),
    ).toBeNull();
  });

  it('returns Completed for idle + unread', () => {
    const pill = resolveThreadStatusPill(
      t({ lastReadAt: 1_000, latestTurnCompletedAt: 2_000 }),
      'idle',
    );
    expect(pill?.label).toBe('Completed');
    expect(pill?.dotClass).toContain('bg-success');
    expect(pill?.pulse).toBe(false);
  });

  it.each([
    ['chat', 'Working'],
    ['plan', 'Planning'],
    ['design', 'Designing'],
    ['discussion', 'Discussing'],
  ] as const)(
    'running + mode=%s resolves to %s',
    (mode, expectedLabel) => {
      const pill = resolveThreadStatusPill(t({ mode }), 'running');
      expect(pill?.label).toBe(expectedLabel);
      expect(pill?.dotClass).toContain('bg-warning');
      expect(pill?.pulse).toBe(true);
    },
  );

  it('running falls back to Working when mode is missing', () => {
    const pill = resolveThreadStatusPill(t({ mode: undefined }), 'running');
    expect(pill?.label).toBe('Working');
  });

  it('pending-approval wins over the running/mode branch', () => {
    const pill = resolveThreadStatusPill(t({ mode: 'plan' }), 'pending-approval');
    expect(pill?.label).toBe('Pending approval');
    expect(pill?.pulse).toBe(true);
    expect(pill?.dotClass).toContain('bg-warning');
  });

  it('awaiting-input renders a pulsing accent pill with accent glow', () => {
    const pill = resolveThreadStatusPill(t(), 'awaiting-input');
    expect(pill?.label).toBe('Awaiting input');
    expect(pill?.pulse).toBe(true);
    expect(pill?.dotClass).toContain('bg-accent');
    expect(pill?.labelClass).toContain('text-accent');
    expect(pill?.glowClass).toBe('status-glow-accent');
  });

  it('pending-approval carries the warning glow', () => {
    const pill = resolveThreadStatusPill(t(), 'pending-approval');
    expect(pill?.glowClass).toBe('status-glow-warning');
  });

  it.each(['error', 'running', 'plan-ready', 'idle'] as const)(
    '%s state carries no glow',
    (status) => {
      const pill = resolveThreadStatusPill(t({ lastReadAt: 1, latestTurnCompletedAt: 2 }), status);
      expect(pill?.glowClass).toBeUndefined();
    },
  );

  it('plan-ready renders a non-pulsing accent pill', () => {
    const pill = resolveThreadStatusPill(t(), 'plan-ready');
    expect(pill?.label).toBe('Plan ready');
    expect(pill?.pulse).toBe(false);
    expect(pill?.dotClass).toContain('bg-accent');
  });

  it('error wins over everything', () => {
    const pill = resolveThreadStatusPill(
      t({ mode: 'plan', lastReadAt: 1_000, latestTurnCompletedAt: 2_000 }),
      'error',
    );
    expect(pill?.label).toBe('Error');
    expect(pill?.dotClass).toContain('bg-error');
    expect(pill?.pulse).toBe(false);
  });

  it('running is picked over unread-completed when both are true', () => {
    const pill = resolveThreadStatusPill(
      t({ lastReadAt: 1_000, latestTurnCompletedAt: 2_000 }),
      'running',
    );
    expect(pill?.label).toBe('Working');
  });
});
