import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

// Cache-hit coverage for the module-level KaTeX cache in the renderer's
// Math component. The proof we need is that the
// second occurrence of the same (token.text, mode) pair does not
// re-invoke `katex.renderToString`.
//
// Important: vitest's Svelte test transform re-evaluates the library
// module per top-level `render()` root (verified by exposing the
// cache and a per-evaluation id sentinel on globalThis — the id list
// grew across two `render()` calls). That means a "mount, unmount,
// remount" pattern can't observe a module-level cache hit under test.
// To prove the cache works, we exercise repetition WITHIN a single
// mount: the source contains the same math block twice, so both
// occurrences share the same module instance and the second occurrence
// hits the cache populated by the first.
//
// In production the module loads once per page and the cache survives
// across ChatMarkdown remounts — which is the case the split-Streamdown
// prefix/tail migration depends on.

const katexCalls: string[] = [];

vi.mock('katex', () => ({
  default: {
    renderToString: vi.fn((code: string) => {
      katexCalls.push(code);
      return `<span class="katex" data-rendered="${code}"></span>`;
    }),
  },
}));

import ChatMarkdown from './ChatMarkdown.svelte';

describe('<ChatMarkdown> KaTeX cache hits within a single render', () => {
  // The cache absorbs the second occurrence — only ONE renderToString call,
  // but TWO rendered nodes (the cached HTML string is reused via @html).
  it.each([
    {
      name: 'renders identical inline $math$ expressions once even when the source contains the same block twice',
      source: '$x_{single-render-inline-1}$ and again $x_{single-render-inline-1}$',
      selector: 'span.katex',
      code: 'x_{single-render-inline-1}',
    },
    {
      name: 'renders identical display $$math$$ blocks once when the source contains the same block twice',
      source: '$$\nE=mc^{single-render-display-1}\n$$\n\n$$\nE=mc^{single-render-display-1}\n$$',
      selector: '.katex',
      code: 'E=mc^{single-render-display-1}',
    },
  ])('$name', async ({ source, selector, code }) => {
    katexCalls.length = 0;
    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      // Both occurrences must appear in the DOM.
      expect(r.container.querySelectorAll(selector).length).toBe(2);
    });
    expect(katexCalls).toEqual([code]);
    r.unmount();
  });
});
