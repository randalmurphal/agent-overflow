import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import TailClampedText from './TailClampedText.svelte';

describe('<TailClampedText>', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders the text and wires id + testId', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'reasoning tail', expanded: false, id: 'row-1', testId: 'body' },
    });
    const el = container.querySelector('[data-testid="body"]');
    expect(el?.textContent).toBe('reasoning tail');
    expect(el?.getAttribute('id')).toBe('row-1');
  });

  it('clamps to 3 lines and bottom-anchors the tail when collapsed', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'x', expanded: false, testId: 'body' },
    });
    const className = container.querySelector('[data-testid="body"]')?.className ?? '';
    expect(className).toMatch(/max-h-\[3lh\]/);
    expect(className).toMatch(/overflow-hidden/);
    // The flex bottom-anchor IS the fix (replaces the old imperative
    // `scrollTop = scrollHeight` pin). Lock it here so its removal fails fast in
    // happy-dom; the real-geometry behaviour is guarded in
    // tailClampedText.browser.test.ts.
    expect(className).toMatch(/justify-end/);
  });

  it('drops the clamp when expanded', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'x', expanded: true, testId: 'body' },
    });
    const className = container.querySelector('[data-testid="body"]')?.className ?? '';
    expect(className).not.toMatch(/max-h-\[3lh\]/);
    expect(className).not.toMatch(/overflow-hidden/);
  });
});
