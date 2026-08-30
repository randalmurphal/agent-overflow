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
 * carries no handler of its own — this intercept is the whole behavior,
 * which is why the mounted case below is covered as well as the DOM one.
 *
 * The pure-DOM cases exercise the handler pattern in isolation (no
 * mermaid module, no async render); the mounted case proves the real
 * component still emits the element the selector depends on.
 */

function attachCaptureHandler(wrapper: HTMLElement): void {
  wrapper.addEventListener(
    'click',
    (e: MouseEvent) => {
      if (!(e.target instanceof Element)) return;
      if (!e.target.closest('[aria-label="Toggle expand"]')) return;
      e.stopPropagation();
      e.preventDefault();
      const svg = wrapper.querySelector('svg[data-mermaid-svg]');
      if (svg) {
        document.dispatchEvent(
          new CustomEvent('diagram-expand', {
            detail: { html: svg.outerHTML },
          }),
        );
      }
    },
    true,
  );
}

function buildDOM(): {
  wrapper: HTMLElement;
  expandBtn: HTMLButtonElement;
  container: HTMLElement;
  svg: SVGSVGElement;
} {
  const wrapper = document.createElement('div');
  wrapper.setAttribute('data-mermaid-source', 'graph TD; A-->B');
  wrapper.className = 'mermaid streamdown-mermaid-host';

  const container = document.createElement('div');
  wrapper.appendChild(container);

  const toolbar = document.createElement('div');
  container.appendChild(toolbar);

  const expandBtn = document.createElement('button');
  expandBtn.setAttribute('aria-label', 'Toggle expand');
  toolbar.appendChild(expandBtn);

  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('data-mermaid-svg', '');
  container.appendChild(svg);

  document.body.appendChild(wrapper);

  return { wrapper, expandBtn, container, svg };
}

describe('mermaid expand intercept', () => {
  it('dispatches diagram-expand on the expand button click', () => {
    const { wrapper, expandBtn } = buildDOM();
    attachCaptureHandler(wrapper);

    const spy = vi.fn();
    document.addEventListener('diagram-expand', spy);

    expandBtn.click();

    expect(spy).toHaveBeenCalledTimes(1);
    const detail = (spy.mock.calls[0][0] as CustomEvent).detail;
    expect(detail.html).toContain('data-mermaid-svg');
    expect(detail.html).not.toContain('aria-label="Toggle expand"');

    document.removeEventListener('diagram-expand', spy);
    wrapper.remove();
  });

  it('prevents propagation so no library handler can fire', () => {
    const { wrapper, expandBtn } = buildDOM();
    attachCaptureHandler(wrapper);

    const bubblingHandler = vi.fn();
    expandBtn.addEventListener('click', bubblingHandler);

    expandBtn.click();

    expect(bubblingHandler).not.toHaveBeenCalled();

    wrapper.remove();
  });

  it('does not intercept clicks elsewhere in the diagram', () => {
    const { wrapper, svg } = buildDOM();
    attachCaptureHandler(wrapper);

    const spy = vi.fn();
    document.addEventListener('diagram-expand', spy);

    svg.dispatchEvent(new MouseEvent('click', { bubbles: true }));

    expect(spy).not.toHaveBeenCalled();

    document.removeEventListener('diagram-expand', spy);
    wrapper.remove();
  });
});

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
