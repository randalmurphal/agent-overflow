import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

// Regression coverage for the source-text fallback inside the real
// math / mermaid hosts. The fallback is the fix for "spring starts
// at the top of the freshly-rendered block" on boundary commit:
// without it, the renderer's Math / Mermaid components render
// an empty inner wrapper until their async `import('katex')` /
// `import('mermaid')` resolves, contentEl dips below the streaming
// bottom, the browser auto-clamps scrollTop, and the stick-to-bottom
// spring chases the entire rendered height from a stale lower
// scrollTop. The fallback holds a hidden source-text pre in the same
// CSS grid cell as the renderer so the wrapper preserves its
// source-text height across the swap; the spring then only chases
// the actual `rendered − source` delta.
//
// What we verify here:
//   1. The block-math host renders an extra `.math-source-fallback`
//      element with the original source text.
//   2. Once KaTeX produces a `.katex` node, the host wrapper picks
//      up the `math-rendered` class so the CSS rules can drop the
//      fallback out of the layout.
//   3. Inline math still uses the lean wrapper (no fallback — the
//      height delta between source text and rendered span is
//      negligible, and inline elements can't host a grid).

const katexCalls: string[] = [];
vi.mock('katex', () => ({
  default: {
    renderToString: vi.fn((code: string) => {
      katexCalls.push(code);
      return `<span class="katex" data-rendered="${encodeURIComponent(code)}"></span>`;
    }),
  },
}));

import ChatMarkdown from './ChatMarkdown.svelte';

describe('<ChatMarkdown> math source-text fallback', () => {
  it('block math wraps the KaTeX render in a fallback grid carrying the source text', async () => {
    katexCalls.length = 0;
    const source = '$$x_{fallback}^2$$';
    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });

    await waitFor(() => {
      const wrapper = r.container.querySelector(
        '.math-display.math-host-with-fallback',
      );
      expect(wrapper).not.toBeNull();
    });

    const wrapper = r.container.querySelector(
      '.math-display.math-host-with-fallback',
    )!;
    expect(wrapper.getAttribute('data-math-source')).toBe('x_{fallback}^2');

    // Fallback pre carrying the unrendered source text — this is the
    // load-bearing piece for the spring distance fix.
    const fallback = wrapper.querySelector('.math-source-fallback');
    expect(fallback).not.toBeNull();
    expect(fallback?.textContent).toBe('x_{fallback}^2');
    expect(fallback?.getAttribute('aria-hidden')).toBe('true');

    // Once KaTeX renders, the wrapper picks up the rendered class so
    // CSS can drop the fallback out of the layout.
    await waitFor(() => {
      expect(wrapper.classList.contains('math-rendered')).toBe(true);
    });

    // The KaTeX render shows up alongside the fallback (both inside
    // the same wrapper — the CSS handles visibility).
    expect(wrapper.querySelector('span.katex')).not.toBeNull();
    expect(katexCalls).toEqual(['x_{fallback}^2']);

    r.unmount();
  });

  it('inline math does not introduce the fallback wrapper', async () => {
    katexCalls.length = 0;
    const source = 'prose with $x_{inline-no-fallback}$ inline';
    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });

    await waitFor(() => {
      expect(r.container.querySelector('span.katex')).not.toBeNull();
    });

    // Inline math keeps the lean span — no grid, no fallback.
    expect(r.container.querySelector('.math-host-with-fallback')).toBeNull();
    const inline = r.container.querySelector('.math-inline');
    expect(inline).not.toBeNull();
    expect(inline?.getAttribute('data-math-source')).toBe('x_{inline-no-fallback}');
    expect(inline?.querySelector('.math-source-fallback')).toBeNull();

    r.unmount();
  });
});
