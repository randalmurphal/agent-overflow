import { beforeAll, describe, expect, it, vi, type MockedFunction } from 'vitest';

// The safe inline elements with no native markdown token (`htmlInline`)
// have two renderers: `render/elements/Element.svelte` on the component
// path and `render/staticHtml.ts` on the compact path that owns every
// settled block. The harness mounts one source through both, so each case
// holds the element to its contract and the two paths to each other.

const TAGS = ['ins', 'mark', 'u', 'small', 'abbr', 'q', 'var', 'cite', 'dfn', 'kbd', 'ruby', 'rt', 'rp'] as const;

// The compact path's serializer, observed, so a case fails when that path
// hands its block back to the component path. The test setup already loads
// the renderer, so the mock reaches it only through a fresh module graph.
vi.mock('../../markdown/render/staticHtml', async (importOriginal) => {
  const original = await importOriginal<typeof import('../../markdown/render/staticHtml')>();
  return { ...original, renderStaticTokenHtml: vi.fn(original.renderStaticTokenHtml) };
});

let fresh: {
  render: typeof import('@testing-library/svelte').render;
  waitFor: typeof import('@testing-library/svelte').waitFor;
  Harness: typeof import('./CompactStaticMarkdownHarness.svelte').default;
  extensions: typeof import('../../markdown').EMBEDDED_HTML_EXTENSIONS;
  serialize: MockedFunction<typeof import('../../markdown/render/staticHtml').renderStaticTokenHtml>;
};
beforeAll(async () => {
  vi.resetModules();
  const library = await import('@testing-library/svelte');
  fresh = {
    render: library.render,
    waitFor: library.waitFor,
    Harness: (await import('./CompactStaticMarkdownHarness.svelte')).default,
    extensions: (await import('../../markdown')).EMBEDDED_HTML_EXTENSIONS,
    serialize: vi.mocked((await import('../../markdown/render/staticHtml')).renderStaticTokenHtml),
  };
  expect(vi.isMockFunction(fresh.serialize)).toBe(true);
});

async function bothPaths(source: string): Promise<[Element, Element]> {
  fresh.serialize.mockClear();
  const { container, unmount } = fresh.render(fresh.Harness, { source, extensions: fresh.extensions });
  try {
    const componentPath = container.querySelector('[data-full-token-tree] > div');
    const staticPath = container.querySelector('[data-compact-static] > div');
    if (!componentPath || !staticPath) throw new Error('differential Streamdown roots did not mount');
    await fresh.waitFor(() => {
      expect(componentPath.textContent).toContain('tail');
      expect(staticPath.textContent).toContain('tail');
    });
    // The compact path served the block itself.
    const served = fresh.serialize.mock.results.map(({ value }) => typeof value);
    expect(served.length).toBeGreaterThan(0);
    expect(served.every((kind) => kind === 'string')).toBe(true);
    return [componentPath.cloneNode(true) as Element, staticPath.cloneNode(true) as Element];
  } finally {
    unmount();
  }
}

describe('inline html elements across both render paths', () => {
  it.each(TAGS)('<%s> renders as itself around its content', async (tag) => {
    const paths = await bothPaths(`lead <${tag} onclick="x()" class="c">held **bold**</${tag}> tail`);
    for (const path of paths) {
      const element = path.querySelector(tag);
      expect(element, tag).not.toBeNull();
      expect(element?.textContent?.trim()).toBe('held bold');
      expect(element?.querySelector('strong')?.textContent).toBe('bold');
      // Nothing authored reaches the element but its content.
      expect(element?.getAttributeNames().filter((name) => !name.startsWith('data-streamdown-'))).toEqual([]);
      expect(path.textContent).not.toContain('<');
    }
  });

  it('an <abbr> carries its title on both paths', async () => {
    const paths = await bothPaths('the <abbr title="Merge &quot;Request&quot;">MR</abbr> tail');
    for (const path of paths) {
      expect(path.querySelector('abbr')?.getAttribute('title')).toBe('Merge "Request"');
    }
  });

  it('a <span> carries its title on both paths, as a forge keeps it', async () => {
    const paths = await bothPaths('a <span title="tooltip" style="color:red">hover</span> tail');
    for (const path of paths) {
      const span = path.querySelector('span[data-streamdown-html-inline]');
      expect(span?.getAttribute('title')).toBe('tooltip');
      expect(span?.hasAttribute('style')).toBe(false);
    }
  });

  it('nested <kbd> renders as keys inside a key, no tag text', async () => {
    const paths = await bothPaths('Press <kbd><kbd>Ctrl</kbd>+<kbd>P</kbd></kbd> tail');
    for (const path of paths) {
      const outer = path.querySelector('p > kbd');
      expect(Array.from(outer?.querySelectorAll(':scope > kbd') ?? [], (key) => key.textContent)).toEqual(['Ctrl', 'P']);
      expect(outer?.textContent).toBe('Ctrl+P');
      expect(path.querySelector('code')).toBeNull();
      expect(path.textContent).not.toContain('<');
    }
  });

  it('an unclosed <b> runs to the end of its paragraph on both paths', async () => {
    const paths = await bothPaths('Unclosed <b>bold *to* the end\n\nnext tail');
    for (const path of paths) {
      expect(path.querySelector('p strong')?.textContent).toBe('bold to the end');
      expect(path.querySelector('p strong em')?.textContent).toBe('to');
      expect(path.querySelectorAll('strong')).toHaveLength(1);
      expect(path.textContent).not.toContain('<');
    }
  });

  it('a <span> and a named anchor are bare spans, and the text around them stays', async () => {
    const paths = await bothPaths('a <span style="color:red">styled</span> and <a name="top">anchored</a> tail');
    for (const path of paths) {
      expect(path.querySelector('span[style], a')).toBeNull();
      expect(Array.from(path.querySelectorAll('span[data-streamdown-html-inline]'), (span) => span.textContent?.trim())).toEqual(['styled', 'anchored']);
      expect(path.textContent?.replace(/\s+/g, ' ').trim()).toBe('a styled and anchored tail');
    }
  });

  it('an <a href> inside a link renders as a span, so the outer link keeps its text', async () => {
    const paths = await bothPaths(
      'lead [outer <span><a href="https://example.invalid/inner">inner</a></span> end](https://example.invalid/outer) tail'
    );
    for (const path of paths) {
      const anchors = Array.from(path.querySelectorAll('a'));
      expect(anchors.map((anchor) => anchor.getAttribute('href'))).toEqual(['https://example.invalid/outer']);
      expect(anchors[0].textContent?.replace(/\s+/g, ' ').trim()).toBe('outer inner end');
      expect(path.textContent?.replace(/\s+/g, ' ').trim()).toBe('lead outer inner end tail');
    }
  });

  it('decoded entity references read the same on both paths', async () => {
    const paths = await bothPaths('A&nbsp;B &lt;tag&gt; &amp; &copy; &notanentity; tail');
    for (const path of paths) {
      expect(path.textContent?.trim()).toBe('A B <tag> & © &notanentity; tail');
    }
  });
});
