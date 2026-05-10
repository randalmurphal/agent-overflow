import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';

import RateLimitMeter from './RateLimitMeter.svelte';
import {
  resetForTest as resetAccountInfoForTest,
  setProviderAccount,
} from '../../stores/accountInfo.svelte';

// Pin Date.now() so the popover countdown text is deterministic.
const NOW_MS = 1_700_000_000_000;
const NOW_SEC = Math.floor(NOW_MS / 1000);

describe('<RateLimitMeter>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
    resetAccountInfoForTest();
  });
  afterEach(() => {
    vi.useRealTimers();
    resetAccountInfoForTest();
  });

  it('renders the static window label inside the ring (not a percent)', () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const button = getByLabelText(/5-hour limit/);
    expect(button.textContent?.trim()).toBe('5h');
  });

  it('derives the 7-hour label and header from windowMins=10080', () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: { entry: null, windowMins: 10080 },
    });
    const button = getByLabelText(/7-day limit/);
    expect(button.textContent?.trim()).toBe('7d');
  });

  it('shows percent + countdown in the popover when entry is present', async () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText('42% used')).toBeTruthy();
    expect(screen.getByText('Resets in 1h')).toBeTruthy();
  });

  it('refreshes countdown text on each hover-open so a stale derived value cannot persist', async () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
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

  it('shows "Awaiting first update" placeholder when entry is null', async () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: { entry: null, windowMins: 10080 },
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
  it('omits the progress-arc circle when entry is null', () => {
    const { container } = render(RateLimitMeter, {
      props: { entry: null, windowMins: 300 },
    });
    const circles = container.querySelectorAll('svg circle');
    // Only the background circle, no progress arc.
    expect(circles.length).toBe(1);
  });

  it('renders the progress arc when entry is present', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
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
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 80, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-fg-subtle');
  });

  it('flips to warning at 81% used', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 81, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-warning');
  });

  it('keeps the warning stroke at exactly 95% used (boundary is exclusive)', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 95, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-warning');
  });

  it('flips to error at 96% used', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 96, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(arc.getAttribute('class')).toContain('stroke-error');
  });

  it('clamps usedPercent to [0, 100] for ring fill so a wire glitch can\'t draw past the circumference', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        // Wildly out-of-range values shouldn't break the SVG dasharray
        // math or the clamp. Neither -50 nor 150 should produce a
        // negative dashoffset (which would render as a longer-than-full
        // arc on some browsers).
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 150, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    const dashOffset = Number(arc.getAttribute('stroke-dashoffset'));
    expect(dashOffset).toBeGreaterThanOrEqual(0);
    // Error color at 100% (clamped from 150).
    expect(arc.getAttribute('class')).toContain('stroke-error');
  });

  it('renders the plan label in the popover when provider + accountInfo are populated', async () => {
    setProviderAccount('claude', { subscriptionType: 'Claude Max' });

    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
        provider: 'claude' as const,
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText('Plan: Claude Max')).toBeTruthy();
  });

  it('omits the plan label when provider is supplied but accountInfo has no subscriptionType', async () => {
    setProviderAccount('codex', { apiProvider: 'openai' }); // no subscriptionType

    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
        provider: 'codex' as const,
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    // Percent line still renders so we know the popover opened.
    expect(await screen.findByText('42% used')).toBeTruthy();
    expect(screen.queryByText(/^Plan:/)).toBeNull();
  });

  it('omits the plan label when no provider is supplied', async () => {
    setProviderAccount('claude', { subscriptionType: 'Claude Max' });

    const { getByLabelText } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));

    expect(await screen.findByText('42% used')).toBeTruthy();
    expect(screen.queryByText(/^Plan:/)).toBeNull();
  });

  it('treats NaN usedPercent as 0% so a non-numeric wire payload renders an empty ring', () => {
    const { container } = render(RateLimitMeter, {
      props: {
        entry: { limitId: 'five_hour', limitName: '5h', usedPercent: NaN, windowMins: 300, resetsAt: NOW_SEC + 3600 },
        windowMins: 300,
      },
    });
    const arc = container.querySelectorAll('svg circle')[1];
    const dashOffset = Number(arc.getAttribute('stroke-dashoffset'));
    expect(Number.isFinite(dashOffset)).toBe(true);
    // 0% → full circumference dashoffset = empty arc.
    expect(arc.getAttribute('class')).toContain('stroke-fg-subtle');
  });
});
