import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';

import SendButton from './SendButton.svelte';

describe('<SendButton>', () => {
  it('shows composer-send when idle and fires onSend on click', async () => {
    const onSend = vi.fn();
    const onInterrupt = vi.fn();
    const { getByTestId, queryByTestId } = render(SendButton, {
      props: { canSend: true, isTurnActive: false, onSend, onInterrupt },
    });
    expect(queryByTestId('composer-interrupt')).toBeNull();
    await fireEvent.click(getByTestId('composer-send'));
    expect(onSend).toHaveBeenCalledOnce();
    expect(onInterrupt).not.toHaveBeenCalled();
  });

  it('is disabled when idle and canSend is false', () => {
    const { getByTestId } = render(SendButton, {
      props: {
        canSend: false,
        isTurnActive: false,
        onSend: vi.fn(),
        onInterrupt: vi.fn(),
      },
    });
    const btn = getByTestId('composer-send') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it('swaps to composer-interrupt and fires onInterrupt when a turn is active', async () => {
    const onSend = vi.fn();
    const onInterrupt = vi.fn();
    const { getByTestId, queryByTestId } = render(SendButton, {
      props: { canSend: false, isTurnActive: true, onSend, onInterrupt },
    });
    expect(queryByTestId('composer-send')).toBeNull();
    const btn = getByTestId('composer-interrupt') as HTMLButtonElement;
    // Interrupt remains enabled even when the textarea is empty.
    expect(btn.disabled).toBe(false);
    await fireEvent.click(btn);
    expect(onInterrupt).toHaveBeenCalledOnce();
    expect(onSend).not.toHaveBeenCalled();
  });
});
