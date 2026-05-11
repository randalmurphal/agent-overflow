import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import RateLimitMeter from './RateLimitMeter.svelte';
import {
  resetForTest as resetRateLimitsInfoForTest,
  setProviderRateLimits,
} from '../../stores/rateLimitsInfo.svelte';
import { resetForTest as resetAccountInfoForTest } from '../../stores/accountInfo.svelte';

const NOW_MS = 1_700_000_000_000;
const NOW_SEC = Math.floor(NOW_MS / 1000);

describe('<RateLimitMeter> reactivity', () => {
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

  it('reflects setProviderRateLimits called AFTER render', async () => {
    const { getByLabelText } = render(RateLimitMeter, {
      props: { windowMins: 300, provider: 'claude' as const },
    });

    // Initially empty: popover should show placeholder.
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));
    expect(await screen.findByText(/Awaiting first update/i)).toBeTruthy();
    await fireEvent.mouseLeave(getByLabelText(/5-hour limit/));

    // Update the store AFTER the component already mounted.
    setProviderRateLimits({
      provider: 'claude',
      limits: [
        { limitId: 'five_hour', limitName: '5h', usedPercent: 42, windowMins: 300, resetsAt: NOW_SEC + 3600 },
      ],
      updatedAt: NOW_SEC,
    });

    await tick();
    await fireEvent.mouseEnter(getByLabelText(/5-hour limit/));
    expect(await screen.findByText('42% used')).toBeTruthy();
  });
});
