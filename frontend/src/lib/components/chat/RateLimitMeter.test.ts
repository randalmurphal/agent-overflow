import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';

import RateLimitMeter from './RateLimitMeter.svelte';
import {
  resetForTest as resetAccountInfoForTest,
  setProviderAccount,
} from '../../stores/accountInfo.svelte';
import {
  resetForTest as resetRateLimitsInfoForTest,
  setProviderRateLimits,
} from '../../stores/rateLimitsInfo.svelte';
import type { RateLimitEntry } from '../../types/events';

// Pin Date.now() so the popover countdown text is deterministic.
const NOW_MS = 1_700_000_000_000;
const NOW_SEC = Math.floor(NOW_MS / 1000);

// Helper: seed the provider-global store with a single entry under
// `provider` so RateLimitMeter's derived lookup resolves to it.
function seedProviderEntry(
  provider: 'claude' | 'codex',
  entry: RateLimitEntry,
): void {
  setProviderRateLimits({
    provider,
    limits: [entry],
    updatedAt: NOW_SEC,
  });
}

describe('<RateLimitMeter>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
    resetAccountInfoForTest();
    resetRateLimitsInfoForTest();
  });
  afterEach(() => {
    vi.useRealTimers();
    resetAccountInfoForTest();
    resetRateLimitsInfoForTest();
  });

  it('renders the static window label inside the ring (not a percent)', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const button = getByLabelText(/5-hour limit/);
    expect(button.textContent?.trim()).toBe('5h');
  });

  it.each([300, 10080])('keeps a tapped %i-minute meter open through synthetic mouseleave and blur', async (windowMins) => {
    const { getByRole, queryByRole } = render(RateLimitMeter, { props: { windowMins, provider: 'claude' } });
    const trigger = getByRole('button');
    await fireEvent.pointerDown(trigger, { pointerType: 'touch' });
    await fireEvent.mouseEnter(trigger);
    await fireEvent.click(trigger);
    await fireEvent.mouseLeave(trigger);
    await fireEvent.blur(trigger);
    await vi.advanceTimersByTimeAsync(200);
    expect(queryByRole('tooltip')).not.toBeNull();
    await fireEvent.mouseDown(document.body);
    expect(queryByRole('tooltip')).toBeNull();
  });

  it('derives the 7-day label and header from windowMins=10080', () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 10080, provider: 'claude' as const },
    });
    const button = getByLabelText(/7-day limit/);
    expect(button.textContent?.trim()).toBe('7d');
  });

  it('shows percent + countdown in the popover when entry is present', async () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText('42% used')).toBeTruthy();
    expect(screen.getByText('Resets in 1h')).toBeTruthy();
  });

  it('lists scoped limits on hover without letting them override the ring', async () => {
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        {
          limitId: 'weekly_scoped:fable',
          limitName: 'Fable',
          usedPercent: 99,
          windowMins: 10080,
          resetsAt: NOW_SEC + 3600,
        },
        {
          limitId: 'weekly_all',
          limitName: 'All models',
          usedPercent: 52,
          windowMins: 10080,
          resetsAt: NOW_SEC + 3600,
        },
      ],
      updatedAt: NOW_SEC,
    });
    const { container, getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 10080, provider: 'claude' as const },
    });

    const button = getByLabelText(/7-day limits: 52% used/i);
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-fg-subtle');

    await fireEvent.mouseEnter(button);
    expect(await screen.findByText('All models')).toBeTruthy();
    expect(screen.getByText('52% used')).toBeTruthy();
    expect(screen.getByText('Fable')).toBeTruthy();
    expect(screen.getByText('99% used')).toBeTruthy();
  });

  it('shows scoped-only details while leaving the account-wide ring empty', async () => {
    setProviderRateLimits({
      provider: 'codex',
      limits: [{
        limitId: 'spark',
        limitName: 'GPT-5.3-Codex-Spark',
        usedPercent: 99,
        windowMins: 300,
        resetsAt: NOW_SEC + 3600,
      }],
      updatedAt: NOW_SEC,
    });
    const { container, getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'codex' as const },
    });

    const button = getByLabelText(/5-hour limit: awaiting first update/i);
    expect(container.querySelectorAll('svg circle')).toHaveLength(1);

    await fireEvent.mouseEnter(button);
    expect(await screen.findByText('GPT-5.3-Codex-Spark')).toBeTruthy();
    expect(screen.getByText('99% used')).toBeTruthy();
  });

  it('labels an unnamed Codex provider-wide bucket as All models', async () => {
    seedProviderEntry('codex', {
      limitId: 'codex',
      limitName: '',
      usedPercent: 42,
      windowMins: 300,
      resetsAt: NOW_SEC + 3600,
    });
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'codex' as const },
    });

    await fireEvent.mouseEnter(getByLabelText(/5-hour limit: 42% used/i));
    expect(await screen.findByText('All models')).toBeTruthy();
  });

  it('refreshes countdown text on each hover-open so a stale derived value cannot persist', async () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const button = getByLabelText(/5-hour limit/);

    await fireEvent.mouseEnter(button);
    expect(await screen.findByText('Resets in 1h')).toBeTruthy();
    await fireEvent.mouseLeave(button);

    // Advance 30 minutes and re-hover. Reset target stays the same wall
    // clock; only the relative remaining time should shrink to 30m.
    vi.setSystemTime(NOW_MS + 30 * 60 * 1000);
    await fireEvent.mouseEnter(button);
    expect(await screen.findByText('Resets in 30m')).toBeTruthy();
  });

  it('shows "Awaiting first update" placeholder when the global store has no entry', async () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 10080, provider: 'claude' as const },
    });
    await fireEvent.mouseEnter(getByLabelText(/7-day limit/));

    expect(await screen.findByText(/Awaiting first update/i)).toBeTruthy();
    // Empty-state must not render a percent.
    expect(screen.queryByText(/% used/)).toBeNull();
  });

  // The fill arc is rendered conditionally so an empty ring shows just
  // the grey background circumference. A regression here would either
  // (a) leak a 0%-fill arc with the linecap rounding artifact, or
  // (b) draw a full ring at the warning color when entry is null.
  it('omits the progress-arc circle when no entry is in the store', () => {
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const circles = container.querySelectorAll('svg circle');
    // Only the background circle, no progress arc.
    expect(circles.length).toBe(1);
  });

  it('renders the progress arc when an entry exists in the store', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const circles = container.querySelectorAll('svg circle');
    expect(circles.length).toBe(2);
  });

  // Threshold boundary cases pin the exact `>` semantics of the
  // palette: 80 is still subtle, 81 is warning; 95 is still warning,
  // 96 is error. Off-by-one mistakes in this palette are easy to ship
  // and visually invisible until the wire happens to land right on a
  // boundary.
  it('keeps the subtle stroke at exactly 80% used (boundary is exclusive)', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 80, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-fg-subtle');
  });

  it('flips to warning at 81% used', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 81, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-warning');
  });

  it('keeps the warning stroke at exactly 95% used (boundary is exclusive)', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-warning');
  });

  it('flips to error at 96% used', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 96, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-error');
  });

  it('clamps usedPercent to [0, 100] for ring fill so a wire glitch can\'t draw past the circumference', () => {
    seedProviderEntry('claude', {
      // Wildly out-of-range values shouldn't break the SVG dasharray
      // math or the clamp. Neither -50 nor 150 should produce a
      // negative dashoffset (which would render as a longer-than-full
      // arc on some browsers).
      limitId: 'five_hour', limitName: '5h', usedPercent: 150, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    const dashOffset = Number(arc.getAttribute('stroke-dashoffset'));
    expect(dashOffset).toBeGreaterThanOrEqual(0);
    // Error color at 100% (clamped from 150).
    expect(arc.getAttribute('class')).toContain('stroke-error');
  });

  it('renders the plan label in the popover when provider + accountInfo are populated', async () => {
    setProviderAccount('claude', { subscriptionType: 'Claude Max' });
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });

    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText('Plan: Claude Max')).toBeTruthy();
  });

  it('renders the live thread account instead of the newly selected account', async () => {
    setProviderAccount(
      'codex',
      { email: 'new@example.com', subscriptionType: 'plus' },
      'account-new',
    );
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'account-old',
      limits: [{
        limitId: 'codex',
        limitName: '5h',
        usedPercent: 17,
        windowMins: 300,
        resetsAt: NOW_SEC + 3600,
      }],
      updatedAt: NOW_SEC,
    });
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'account-new',
      limits: [{
        limitId: 'codex',
        limitName: '5h',
        usedPercent: 83,
        windowMins: 300,
        resetsAt: NOW_SEC + 3600,
      }],
      updatedAt: NOW_SEC,
    });

    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        windowMins: 300,
        provider: 'codex' as const,
        accountId: 'account-old',
        accountEmail: 'old@example.com',
        subscriptionType: 'pro',
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit: 17% used/i));

    expect(await screen.findByText('17% used')).toBeTruthy();
    expect(screen.getByText('Plan: pro')).toBeTruthy();
    expect(screen.getByText('Account: old@example.com')).toBeTruthy();
    expect(screen.queryByText('83% used')).toBeNull();
  });

  it('does not label an explicit session account with selected-account metadata', async () => {
    setProviderAccount(
      'codex',
      { email: 'selected@example.com', subscriptionType: 'pro' },
      'selected-account',
    );
    setProviderRateLimits({
      provider: 'codex',
      accountId: 'session-account',
      limits: [{
        limitId: 'codex',
        limitName: '5h',
        usedPercent: 12,
        windowMins: 300,
        resetsAt: NOW_SEC + 3600,
      }],
      updatedAt: NOW_SEC,
    });

    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        windowMins: 300,
        provider: 'codex' as const,
        accountId: 'session-account',
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit: 12% used/i));

    expect(await screen.findByText('12% used')).toBeTruthy();
    expect(screen.queryByText('Plan: pro')).toBeNull();
    expect(screen.queryByText('Account: selected@example.com')).toBeNull();
  });

  it('omits the plan label when provider is supplied but accountInfo has no subscriptionType', async () => {
    setProviderAccount('codex', { apiProvider: 'openai' }); // no subscriptionType
    seedProviderEntry('codex', {
      limitId: 'codex', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });

    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'codex' as const },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    // Percent line still renders so we know the popover opened.
    expect(await screen.findByText('42% used')).toBeTruthy();
    expect(screen.queryByText(/^Plan:/)).toBeNull();
  });

  it('omits the plan label when no provider is supplied', async () => {
    // Provider undefined — no global lookup, no plan line. Component
    // gracefully renders the empty-state ring.
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300 },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText(/Awaiting first update/i)).toBeTruthy();
    expect(screen.queryByText(/^Plan:/)).toBeNull();
  });

  it('treats NaN usedPercent as 0% so a non-numeric wire payload renders an empty ring', () => {
    seedProviderEntry('claude', {
      limitId: 'five_hour', limitName: '5h', usedPercent: NaN, windowMins: 300, resetsAt: NOW_SEC + 3600,
    });
    const { container } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    const dashOffset = Number(arc.getAttribute('stroke-dashoffset'));
    expect(Number.isFinite(dashOffset)).toBe(true);
    // 0% → full circumference dashoffset = empty arc.
    expect(arc.getAttribute('class')).toContain('stroke-fg-subtle');
  });
});
