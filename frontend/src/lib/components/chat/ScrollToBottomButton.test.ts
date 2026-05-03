import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ScrollToBottomButton from './ScrollToBottomButton.svelte';

describe('<ScrollToBottomButton>', () => {
  it('renders when visible is true', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();
  });

  it('does not render when visible is false', () => {
    const { queryByTestId } = render(ScrollToBottomButton, {
      props: { visible: false, onClick: () => {} },
    });
    expect(queryByTestId('scroll-to-bottom')).toBeNull();
  });

  it('invokes onClick when clicked', async () => {
    const onClick = vi.fn();
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick },
    });
    await fireEvent.click(getByTestId('scroll-to-bottom'));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('exposes an aria-label and title for accessibility', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    expect(button.getAttribute('aria-label')).toBe('Scroll to latest');
    expect(button.getAttribute('title')).toBe('Scroll to latest');
  });

  it('floats above the composer overlay (z-30 + bottom tracks --composer-height)', () => {
    // Regression: when the chip used `bottom-4 z-10` it was visually
    // and click-wise covered by the composer overlay (z-20, grows
    // upward from bottom-0). The fix lifts the chip via the
    // --composer-height CSS variable and bumps z-index above the
    // composer.
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    const style = button.getAttribute('style') ?? '';
    expect(style).toContain('--composer-height');
    expect(button.className).toContain('z-30');
    // Positioning is anchored, not a transient default.
    expect(button.className).not.toContain('bottom-4');
  });
});
