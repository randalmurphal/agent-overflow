import { describe, expect, it } from 'vitest';
import type { Thread } from '../types/models';
import { hasUnread, resolveEffectiveThreadStatus, resolveThreadStatusPill } from './threadStatusPill';

type MinimalThread = Pick<
  Thread,
  | 'mode'
  | 'lastReadAt'
  | 'latestTurnCompletedAt'
  | 'hasIncompleteTurn'
  | 'hasActionableProposedPlan'
  | 'worktreeSetupState'
>;

function t(overrides: Partial<MinimalThread> = {}): MinimalThread {
  return {
    mode: 'chat',
    lastReadAt: undefined,
    latestTurnCompletedAt: 1_000,
    hasIncompleteTurn: false,
    hasActionableProposedPlan: false,
    worktreeSetupState: '',
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

describe('resolveEffectiveThreadStatus', () => {
  it('keeps live status above durable boot status', () => {
    expect(resolveEffectiveThreadStatus(t({ hasIncompleteTurn: true }), 'running')).toBe('running');
    expect(resolveEffectiveThreadStatus(t({ hasActionableProposedPlan: true }), 'error')).toBe('error');
  });

  it('restores interrupted from an incomplete latest turn when idle', () => {
    expect(resolveEffectiveThreadStatus(t({ hasIncompleteTurn: true }), 'idle')).toBe('interrupted');
  });

  it('does not infer interrupted while authoritative live state is hydrating', () => {
    expect(
      resolveEffectiveThreadStatus(t({ hasIncompleteTurn: true }), 'idle', {
        suppressDurableInterrupted: true,
      }),
    ).toBe('idle');
  });

  it('restores plan-ready from an actionable proposed plan when idle', () => {
    expect(resolveEffectiveThreadStatus(t({ hasActionableProposedPlan: true }), 'idle')).toBe('plan-ready');
  });

  it('prefers interrupted over plan-ready when both durable flags are present', () => {
    expect(
      resolveEffectiveThreadStatus(t({
        hasIncompleteTurn: true,
        hasActionableProposedPlan: true,
      }), 'idle'),
    ).toBe('interrupted');
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
      if (mode === 'discussion') {
        expect(pill?.dotClass).toContain('border-info');
        expect(pill?.dotClass).toContain('bg-transparent');
        expect(pill?.labelClass).toContain('text-info');
        expect(pill?.pulse).toBe(false);
        return;
      }
      expect(pill?.dotClass).toContain('bg-success');
      expect(pill?.pulse).toBe(true);
    },
  );

  it('running falls back to Working when mode is missing', () => {
    const pill = resolveThreadStatusPill(t({ mode: undefined }), 'running');
    expect(pill?.label).toBe('Working');
  });

  it('pending-approval wins over the running/mode branch', () => {
    const pill = resolveThreadStatusPill(t({ mode: 'plan' }), 'pending-approval');
    expect(pill?.label).toBe('Pending Approval');
    expect(pill?.pulse).toBe(true);
    expect(pill?.dotClass).toContain('bg-warning');
  });

  it('awaiting-input renders a pulsing info pill with info glow', () => {
    const pill = resolveThreadStatusPill(t(), 'awaiting-input');
    expect(pill?.label).toBe('Awaiting Input');
    expect(pill?.pulse).toBe(true);
    expect(pill?.dotClass).toContain('bg-info');
    expect(pill?.labelClass).toContain('text-info');
    expect(pill?.glowClass).toBe('status-glow-info');
  });

  it('pending-approval carries the warning glow', () => {
    const pill = resolveThreadStatusPill(t(), 'pending-approval');
    expect(pill?.glowClass).toBe('status-glow-warning');
  });

  it.each(['error', 'running', 'plan-ready', 'interrupted', 'idle'] as const)(
    '%s state carries no glow',
    (status) => {
      const pill = resolveThreadStatusPill(t({ lastReadAt: 1, latestTurnCompletedAt: 2 }), status);
      expect(pill?.glowClass).toBeUndefined();
    },
  );

  it('plan-ready renders a non-pulsing accent pill', () => {
    const pill = resolveThreadStatusPill(t(), 'plan-ready');
    expect(pill?.label).toBe('Plan Ready');
    expect(pill?.pulse).toBe(false);
    expect(pill?.dotClass).toContain('bg-accent');
  });

  it('interrupted renders a non-pulsing warning pill', () => {
    const pill = resolveThreadStatusPill(t(), 'interrupted');
    expect(pill?.label).toBe('Interrupted');
    expect(pill?.pulse).toBe(false);
    expect(pill?.dotClass).toContain('bg-warning');
  });

  it('error wins over everything', () => {
    const pill = resolveThreadStatusPill(
      t({ mode: 'plan', lastReadAt: 1_000, latestTurnCompletedAt: 2_000 }),
      'error',
    );
    expect(pill?.label).toBe('Failed');
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

describe('setup-failed', () => {
  const failedSetup = { worktreeSetupState: 'failed' };

  // The single most important property: a durable provisioning failure must
  // never hide a live status the user is actually blocked on. The resolver's
  // early return on a non-idle live status is what makes this structural, so
  // the whole non-idle set is asserted rather than a sample.
  it.each([
    'error',
    'pending-approval',
    'awaiting-input',
    'running',
  ] as const)('never masks live status %s', (liveStatus) => {
    expect(resolveEffectiveThreadStatus(t(failedSetup), liveStatus)).toBe(liveStatus);
  });

  it('resolves from the durable row state when idle', () => {
    expect(resolveEffectiveThreadStatus(t(failedSetup), 'idle')).toBe('setup-failed');
  });

  it('outranks the other durable fallbacks', () => {
    expect(
      resolveEffectiveThreadStatus(t({
        ...failedSetup,
        hasIncompleteTurn: true,
        hasActionableProposedPlan: true,
      }), 'idle'),
    ).toBe('setup-failed');
  });

  // 'running' is deliberately not a pill: a setup in flight is shown in the
  // pane, and a second sidebar state for it would compete with the turn status.
  it.each(['', 'running', undefined])('shows nothing for worktreeSetupState %p', (state) => {
    expect(
      resolveEffectiveThreadStatus(t({ worktreeSetupState: state }), 'idle'),
    ).toBe('idle');
  });

  it('renders a warning pill, not an error pill', () => {
    const pill = resolveThreadStatusPill(t(), 'setup-failed');
    expect(pill).toEqual({
      label: 'Setup Failed',
      dotClass: 'bg-warning',
      labelClass: 'text-warning',
      pulse: false,
    });
    // No glow: nothing is blocked waiting on the user, unlike an approval.
    expect(pill?.glowClass).toBeUndefined();
  });

  it('is distinct from the failed-turn pill', () => {
    expect(resolveThreadStatusPill(t(), 'error')?.label).toBe('Failed');
    expect(resolveThreadStatusPill(t(), 'setup-failed')?.label).toBe('Setup Failed');
  });
});
