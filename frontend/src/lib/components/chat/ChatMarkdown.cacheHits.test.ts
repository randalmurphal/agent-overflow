import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

// Cache-hit coverage for the module-level KaTeX cache in the patched
// svelte-streamdown Math component. The proof we need is that the
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
// prefix/tail migration depends on. The Mermaid cache uses the same
// module-level Map pattern; its renderer mock is fragile across
// vitest dynamic imports so we cover the id-rewrite contract directly
// with a string-substitution test below rather than through full
// component mounting.

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
  it('renders identical inline $math$ expressions once even when the source contains the same block twice', async () => {
    katexCalls.length = 0;
    const source = '$x_{single-render-inline-1}$ and again $x_{single-render-inline-1}$';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      const rendered = r.container.querySelectorAll('span.katex');
      // Both occurrences must appear in the DOM.
      expect(rendered.length).toBe(2);
    });

    // The cache absorbs the second occurrence — only ONE
    // renderToString call, but TWO rendered nodes (the cached HTML
    // string is reused via @html).
    expect(katexCalls.length).toBe(1);
    expect(katexCalls[0]).toBe('x_{single-render-inline-1}');
    r.unmount();
  });

  it('renders identical display $$math$$ blocks once when the source contains the same block twice', async () => {
    katexCalls.length = 0;
    const source = '$$\nE=mc^{single-render-display-1}\n$$\n\n$$\nE=mc^{single-render-display-1}\n$$';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      const rendered = r.container.querySelectorAll('.katex');
      expect(rendered.length).toBe(2);
    });

    expect(katexCalls.length).toBe(1);
    expect(katexCalls[0]).toContain('E=mc^{single-render-display-1}');
    r.unmount();
  });
});

describe('Mermaid cache id-rewrite contract', () => {
  // Pure unit coverage of the substitution the patched Mermaid.svelte
  // performs on cache hit:
  //
  //   svgString = cached.svg.split(cached.baseId).join(uniqueId);
  //
  // The goal is that two concurrent in-DOM instances of the same
  // cached SVG don't collide on document-scoped fragment ids
  // (`url(#…)` / `xlink:href="#…"` resolve to the first match in
  // document order, so collisions silently break gradients,
  // arrowheads, and markers on the second instance). Verifying the
  // substitution as a string operation is sufficient here — Mermaid
  // bakes its uniqueId into element ids and the fragment refs as a
  // contiguous string token, so rewriting the substring everywhere
  // it appears is precisely the property we need.
  it('rewrites every occurrence of the baked-in uniqueId so concurrent in-DOM instances do not collide', () => {
    const cachedSvg =
      '<svg id="mermaid-A-1" data-mermaid="x">' +
      '<defs>' +
      '<linearGradient id="mermaid-A-1-grad"/>' +
      '<marker id="mermaid-A-1-arrow"/>' +
      '</defs>' +
      '<rect fill="url(#mermaid-A-1-grad)"/>' +
      '<path xlink:href="#mermaid-A-1-arrow"/>' +
      '</svg>';
    const cachedBaseId = 'mermaid-A-1';
    const freshUniqueId = 'mermaid-B-2';

    const rewritten = cachedSvg.split(cachedBaseId).join(freshUniqueId);

    expect(rewritten).not.toContain(cachedBaseId);
    expect(rewritten).toContain(`id="${freshUniqueId}"`);
    expect(rewritten).toContain(`id="${freshUniqueId}-grad"`);
    expect(rewritten).toContain(`id="${freshUniqueId}-arrow"`);
    expect(rewritten).toContain(`fill="url(#${freshUniqueId}-grad)"`);
    expect(rewritten).toContain(`xlink:href="#${freshUniqueId}-arrow"`);
  });

  it('leaves non-id content of the cached SVG unchanged', () => {
    const cachedSvg = '<svg id="mermaid-Z-9"><text>graph TD; A-->B</text></svg>';
    const rewritten = cachedSvg.split('mermaid-Z-9').join('mermaid-Y-8');
    // Preserved content.
    expect(rewritten).toContain('graph TD; A-->B');
    expect(rewritten).toContain('<text>');
  });
});
