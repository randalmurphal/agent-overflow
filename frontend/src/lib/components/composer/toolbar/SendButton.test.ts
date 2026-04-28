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

  it('shows the stop variant during the optimistic sendInFlight window', async () => {
    const onSend = vi.fn();
    const onInterrupt = vi.fn();
    // sendInFlight=true with isTurnActive=false simulates the dispatch
    // window between user-Send and `provider:turn_started`. The stop
    // button must already be visible so Esc / click can abort.
    const { getByTestId, queryByTestId } = render(SendButton, {
      props: { canSend: false, isTurnActive: false, sendInFlight: true, onSend, onInterrupt },
    });
    expect(queryByTestId('composer-send')).toBeNull();
    const btn = getByTestId('composer-interrupt') as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    await fireEvent.click(btn);
    expect(onInterrupt).toHaveBeenCalledOnce();
    expect(onSend).not.toHaveBeenCalled();
  });

  it('shows the send-without-comments menu for plan comment sends', async () => {
    const onSendWithoutPlanComments = vi.fn();
    const { getByTestId, findByText } = render(SendButton, {
      props: {
        canSend: true,
        isTurnActive: false,
        action: 'send-comments',
        label: 'Send comments',
        onSend: vi.fn(),
        onSendWithoutPlanComments,
        onInterrupt: vi.fn(),
      },
    });

    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Send without comments'));

    expect(onSendWithoutPlanComments).toHaveBeenCalledOnce();
  });

  it('shows a comment count badge for plan comment sends', () => {
    const { getByTestId, getByText } = render(SendButton, {
      props: {
        canSend: true,
        isTurnActive: false,
        action: 'send-comments',
        label: 'Send comments',
        planCommentCount: 3,
        onSend: vi.fn(),
        onInterrupt: vi.fn(),
      },
    });

    expect(getByTestId('composer-send')).toHaveTextContent('Send comments 3');
    expect(getByText('3')).toBeInTheDocument();
  });
});
