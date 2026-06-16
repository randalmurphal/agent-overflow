// Verifies the shared tool-row status wrapper contract.
//
// The load-bearing assertion is `inline-flex`: the wrapper MUST
// establish a flex context, never a plain inline box. The running dot
// is a 6px inline-block; in a plain inline span it would sit in a line
// box sized by the inherited 24px line-height, making a running tool row
// ~7px taller than its settled height. On completion the dot is removed
// and the row snaps shorter — and because the timeline is bottom-pinned,
// that shrink is compensated instantly and reads as a small scroll
// shift. (Diagnosed from a real UI trace: every tool row resized 32→25px
// at completion.) `inline-flex` makes the wrapper track the dot's real
// height so running and settled rows match. Asserting the class is the
// enforceable guard here — happy-dom reports zero geometry, so a pixel
// test can't run in this suite; the end-to-end pixel proof lives in the
// live-CSS measurement noted on the fix.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';

describe('<ToolRowStatusIndicator>', () => {
  it('renders nothing when state is null (settled/idle success)', () => {
    const { container } = render(ToolRowStatusIndicator, {
      props: { item: { status: 'completed' }, state: null, testId: 'row-status' },
    });
    expect(container.querySelector('[data-testid="row-status"]')).toBeNull();
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('wraps the dot in a flex context so a running row is not line-box inflated', () => {
    const { getByTestId } = render(ToolRowStatusIndicator, {
      props: { item: { status: 'running' }, state: 'running', testId: 'row-status' },
    });
    const wrapper = getByTestId('row-status');
    // The fix: a flex wrapper can't form a tall text line box around the
    // 6px dot. If this regresses to a plain inline span the running row
    // grows ~7px and shifts on completion.
    expect(wrapper.className).toContain('inline-flex');
    // Contract attributes preserved for tests/styling.
    expect(wrapper.getAttribute('data-status')).toBe('running');
    expect(wrapper.getAttribute('data-state')).toBe('running');
    // Accessibility lives on the inner dot, not the wrapper: the dot is
    // the role="status" live region a screen reader announces. The
    // wrapper must NOT carry a redundant aria-label (two labels for one
    // dot) — asserting its absence locks that contract.
    expect(wrapper.hasAttribute('aria-label')).toBe(false);
    const dot = wrapper.querySelector('[data-testid="indicator"]');
    expect(dot).not.toBeNull();
    expect(dot?.getAttribute('role')).toBe('status');
    expect(dot?.getAttribute('aria-label')).toBe('Running');
  });

  it('omits data-testid (no literal "undefined") when testId is not supplied', () => {
    const { container } = render(ToolRowStatusIndicator, {
      props: { item: { status: 'errored' }, state: 'error' },
    });
    // The wrapper is the component's root element. Anchor to it directly
    // rather than a [data-state] selector: the inner Indicator dot also
    // carries data-state, so the selector would match both and pass only
    // by document order.
    const wrapper = container.firstElementChild as HTMLElement;
    expect(wrapper).not.toBeNull();
    expect(wrapper.className).toContain('inline-flex');
    expect(wrapper.hasAttribute('data-testid')).toBe(false);
    expect(wrapper.getAttribute('data-status')).toBe('errored');
  });
});
