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

  it('carries data-scroll-anchor-ignore so the click-anchor handler skips it', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    expect(button.hasAttribute('data-scroll-anchor-ignore')).toBe(true);
  });

  it('exposes an aria-label and title for accessibility', () => {
    const { getByTestId } = render(ScrollToBottomButton, {
      props: { visible: true, onClick: () => {} },
    });
    const button = getByTestId('scroll-to-bottom');
    expect(button.getAttribute('aria-label')).toBe('Scroll to latest');
    expect(button.getAttribute('title')).toBe('Scroll to latest');
  });
});
