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

  it('uses its visible label as the accessible name', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    expect(button).toHaveTextContent('Jump to bottom');
    expect(button).toHaveAccessibleName('Jump to bottom');
  });

  it('floats above the composer overlay (z-30 + bottom tracks --composer-height)', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    const overlay = button.parentElement!;
    const style = overlay.getAttribute('style') ?? '';
    expect(style).toContain('--composer-height');
    expect(overlay.className).toContain('z-30');
    // Positioning is anchored, not a transient default.
    expect(overlay.className).not.toContain('bottom-4');
  });
});
