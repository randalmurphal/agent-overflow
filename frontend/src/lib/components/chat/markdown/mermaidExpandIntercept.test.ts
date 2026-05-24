import { describe, expect, it, vi } from 'vitest';

/**
 * Verifies the capture-phase intercept that routes the panzoom "Toggle
 * expand" button to DiagramModal instead of the library's broken
 * position:fixed overlay (which lands off-screen inside virtua's
 * transform-containing rows).
 *
 * These are pure DOM tests — they don't mount the full Svelte component
 * because that requires the mermaid library. The handler logic under
 * test is the same pattern wired via `onclickcapture` in
 * StreamdownMermaidHost.svelte.
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

function buildDOM(): { wrapper: HTMLElement; expandBtn: HTMLButtonElement; container: HTMLElement } {
  const wrapper = document.createElement('div');
  wrapper.setAttribute('data-mermaid-source', 'graph TD; A-->B');
  wrapper.className = 'mermaid streamdown-mermaid-host';

  const container = document.createElement('div');
  container.setAttribute('data-expanded', 'false');
  wrapper.appendChild(container);

  const toolbar = document.createElement('div');
  container.appendChild(toolbar);

  for (const label of ['Zoom to fit', 'Zoom in', 'Zoom out', 'Toggle expand']) {
    const btn = document.createElement('button');
    btn.setAttribute('aria-label', label);
    toolbar.appendChild(btn);
  }

  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('data-mermaid-svg', '');
  container.appendChild(svg);

  document.body.appendChild(wrapper);

  const expandBtn = toolbar.querySelector<HTMLButtonElement>(
    '[aria-label="Toggle expand"]',
  )!;

  return { wrapper, expandBtn, container };
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

  it('prevents propagation so panzoom.toggleExpand never fires', () => {
    const { wrapper, expandBtn } = buildDOM();
    attachCaptureHandler(wrapper);

    const bubblingHandler = vi.fn();
    expandBtn.addEventListener('click', bubblingHandler);

    expandBtn.click();

    expect(bubblingHandler).not.toHaveBeenCalled();

    wrapper.remove();
  });

  it('does not intercept clicks on other toolbar buttons', () => {
    const { wrapper } = buildDOM();
    attachCaptureHandler(wrapper);

    const spy = vi.fn();
    document.addEventListener('diagram-expand', spy);

    const zoomBtn = wrapper.querySelector<HTMLButtonElement>('[aria-label="Zoom in"]')!;
    zoomBtn.click();

    expect(spy).not.toHaveBeenCalled();

    document.removeEventListener('diagram-expand', spy);
    wrapper.remove();
  });

  it('does not set data-expanded to true', () => {
    const { wrapper, expandBtn, container } = buildDOM();
    attachCaptureHandler(wrapper);

    expandBtn.click();

    expect(container.dataset.expanded).toBe('false');
    wrapper.remove();
  });
});
