import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';

import SendNowButton from './SendNowButton.svelte';

describe('<SendNowButton>', () => {
  it('renders nothing when no turn is active', () => {
    const { queryByTestId } = render(SendNowButton, {
      props: {
        isTurnActive: false,
        hasQueuedItems: true,
        onInterrupt: vi.fn(),
      },
    });
    expect(queryByTestId('composer-send-now')).toBeNull();
  });

  it('renders nothing when no items are queued', () => {
    const { queryByTestId } = render(SendNowButton, {
      props: {
        isTurnActive: true,
        hasQueuedItems: false,
        onInterrupt: vi.fn(),
      },
    });
    expect(queryByTestId('composer-send-now')).toBeNull();
  });

  it('renders only when a turn is active AND items are queued', () => {
    const { getByTestId } = render(SendNowButton, {
      props: {
        isTurnActive: true,
        hasQueuedItems: true,
        onInterrupt: vi.fn(),
      },
    });
    const btn = getByTestId('composer-send-now') as HTMLButtonElement;
    expect(btn.getAttribute('aria-label')).toBe('Send queued messages now');
    expect(btn.textContent).toContain('Send Now');
  });

  it('fires onInterrupt on click', async () => {
    const onInterrupt = vi.fn();
    const { getByTestId } = render(SendNowButton, {
      props: {
        isTurnActive: true,
        hasQueuedItems: true,
        onInterrupt,
      },
    });
    await fireEvent.click(getByTestId('composer-send-now'));
    expect(onInterrupt).toHaveBeenCalledOnce();
  });
});
