import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import StreamdownHostSourceSwapHarness from '../StreamdownHostSourceSwapHarness.svelte';

/**
 * Verifies the capture-phase intercept that routes the renderer's Mermaid
 * component's "Toggle expand" button to `DiagramModal`.
 *
 * The button is the ONLY diagram control the library still renders: the
 * inline panzoom (zoom in / out / fit) and download chrome were deleted,
 * because a `position: fixed` overlay lands off-screen inside the
 * virtualizer's containment-scoped rows and `DiagramInteractionHost` +
 * `DiagramModal` own zoom, pan and copy anyway. The button therefore
 * carries no handler of its own — this intercept is the whole behavior.
 */

describe('mounted mermaid host', () => {
  it('renders exactly one diagram control and routes it to the expand event', async () => {
    const spy = vi.fn();
    document.addEventListener('diagram-expand', spy);

    const view = render(StreamdownHostSourceSwapHarness, {
      props: { kind: 'mermaid', source: 'graph TD\n  A[start] --> B[end]' },
    });

    await waitFor(() => {
      expect(view.container.querySelector('[aria-label="Toggle expand"]')).not.toBeNull();
    });

    // The panzoom toolbar is gone: no zoom, fit-view or download control
    // survives inside the rendered diagram.
    const controls = Array.from(
      view.container.querySelectorAll('[data-streamdown-mermaid] button'),
      (button) => button.getAttribute('aria-label'),
    );
    expect(controls).toEqual(['Toggle expand']);

    view.container
      .querySelector<HTMLButtonElement>('[aria-label="Toggle expand"]')!
      .click();

    expect(spy).toHaveBeenCalledTimes(1);
    expect((spy.mock.calls[0][0] as CustomEvent).detail.html).toContain('data-mermaid-svg');

    document.removeEventListener('diagram-expand', spy);
    view.unmount();
  });
});
