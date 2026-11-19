import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import {
  EMBEDDED_HTML_EXTENSIONS,
  createDocumentContextCache,
  createParseBlocksCache,
  createProvenAppend,
  documentContextOf,
  parseBlocks,
  type DocumentForge,
} from '../../markdown';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Constructs that need the whole document: reference definitions on every
// surface; a forge's footnotes, heading anchors, `#name` links and
// GitLab's table of contents on review surfaces only.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const FORGES = [['GitHub', GITHUB], ['GitLab', GITLAB]] as const;

const hrefs = (root: Element, selector = 'a[href]') => [...root.querySelectorAll(selector)].map((a) => a.getAttribute('href'));
const anchors = (root: Element) => [...root.querySelectorAll('[data-md-anchor]')].map((node) => node.getAttribute('data-md-anchor'));

const REFERENCES = 'Use [first] and [Second  Label][] here.\n\n> Quoted [first]\n\n[first]: https://example.invalid/first\n\n[second label]: https://example.invalid/second "Second"\n\nlast line\n';

describe('reference definitions', () => {
  it.each([
    ['agent chat', null, false],
    ['agent chat while streaming', null, true],
    ['a GitHub body', GITHUB, false],
    ['a GitLab body', GITLAB, false],
  ] as const)('resolve across blocks in %s', async (_name, forgeSource, streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: REFERENCES, forgeSource, streaming, embeddedHtml: forgeSource !== null } });
    await waitFor(() => expect(container.textContent).toContain('last line'));
    expect(hrefs(container)).toEqual(['https://example.invalid/first', 'https://example.invalid/second', 'https://example.invalid/first']);
    expect(container.querySelectorAll('a[title="Second"]')).toHaveLength(1);
    expect(container.textContent).not.toContain('[first]');
  });

  it('resolve a use once its definition commits, rebuilding only the blocks that use it', async () => {
    // A static block, a citation island and a block that uses nothing.
    const start = 'Use [late][] here.\n\nCite [late] here.\n\nUnrelated paragraph.\n\nFourth paragraph.\n\n';
    const view = render(ChatMarkdown, { props: { source: start, streaming: true } });
    await waitFor(() => expect(view.container.querySelector('.md-committed')?.textContent).toContain('Unrelated paragraph.'));
    const [use, cite, unrelated] = view.container.querySelectorAll('.md-committed p');
    expect(use.textContent).toBe('Use [late][] here.');
    expect(cite.textContent).toBe('Cite [late] here.');

    let source = start;
    for (const delta of ['[late]: https://example.invalid/late\n\n', 'After the definition.\n\n', 'Tail.']) {
      const next = source + delta;
      await view.rerender({ source: next, sourceAppend: createProvenAppend(source, delta) });
      source = next;
    }
    await waitFor(() => expect(view.container.querySelectorAll('a[href="https://example.invalid/late"]')).toHaveLength(2));
    const paragraphs = view.container.querySelectorAll('.md-committed p');
    expect(paragraphs[0]).not.toBe(use);
    expect(paragraphs[0].textContent).toBe('Use late here.');
    expect(paragraphs[1].textContent).toBe('Cite late here.');
    expect(paragraphs[2]).toBe(unrelated);
  });

  it('resolve in the volatile tail from a definition the committed blocks hold', async () => {
    const source = '[ref]: https://example.invalid/ref\n\nCommitted paragraph.\n\nTail uses [ref]';
    const view = render(ChatMarkdown, { props: { source, streaming: true } });
    await waitFor(() => expect(view.container.querySelector('.md-volatile')?.textContent).toContain('Tail uses'));
    expect(hrefs(view.container.querySelector('.md-volatile')!)).toEqual(['https://example.invalid/ref']);
  });
});

describe('forge footnotes', () => {
  const SOURCE = 'One[^b] two[^a] three[^b] four[^missing].\n\n[^a]: Note **a**.\n[^b]: Note b.\n[^unused]: Unused.\n\nlast line\n';

  it.each(FORGES)('number references and list every body after the %s body', async (_name, forgeSource) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource } });
    await waitFor(() => expect(container.querySelector('.md-footnotes')).not.toBeNull());
    const chips = [...container.querySelectorAll('.md-committed [data-footnote-label]')];
    expect(chips.map((chip) => [chip.getAttribute('data-footnote-label'), chip.textContent?.trim(), chip.getAttribute('data-md-anchor')])).toEqual([
      ['b', '1', 'fnref:1'],
      ['a', '2', 'fnref:2'],
      ['b', '1', 'fnref:1:2'],
    ]);
    expect(container.querySelector('.md-committed')?.textContent).toContain('four[^missing].');
    const items = [...container.querySelectorAll('.md-footnotes li')];
    expect(items.map((item) => item.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Note b. ↩ ↩²', 'Note a. ↩']);
    expect(hrefs(items[0], 'a[data-md-fragment]')).toEqual(['#fnref:1', '#fnref:1:2']);
    expect(container.querySelector('.md-footnotes strong')?.textContent).toBe('a');
    expect(container.textContent).not.toContain('Unused');
  });

  it('keep their labels and no section in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOURCE } });
    await waitFor(() => expect(container.textContent).toContain('last line'));
    expect([...container.querySelectorAll('[data-footnote-label]')].map((chip) => chip.textContent?.trim())).toEqual(['b', 'a', 'b', 'missing']);
    expect(container.querySelector('.md-footnotes')).toBeNull();
    expect(container.querySelector('[data-md-anchor]')).toBeNull();
  });
});

const SECTIONS = '[[_TOC_]]\n\n# Intro\n\n## Usage\n\n## Usage\n\n<a name="named"></a><a id="ided"></a>\n\n[jump](#usage-1) [named](#named)\n';

describe('heading anchors and same-document links', () => {
  it.each(FORGES)('anchor headings and named targets and make `#name` links jumpable on %s', async (name, forgeSource) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SECTIONS, forgeSource } });
    await waitFor(() => expect(container.querySelector('a[data-md-fragment]')).not.toBeNull());
    const named = name === 'GitHub' ? ['named', 'ided'] : ['named'];
    expect(anchors(container)).toEqual(['intro', 'usage', 'usage-1', ...named]);
    expect(hrefs(container, 'p a[data-md-fragment]')).toEqual(['#usage-1', '#named']);
    for (const link of container.querySelectorAll('a[data-md-fragment]')) expect(link.getAttribute('target')).toBeNull();
  });

  it('leave agent chat headings unanchored and its `#name` links as references', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SECTIONS } });
    await waitFor(() => expect(container.textContent).toContain('jump'));
    expect(container.querySelector('[data-md-anchor], a[data-md-fragment]')).toBeNull();
    expect(container.querySelector('[data-streamdown-link-blocked][title="#usage-1"]')).not.toBeNull();
  });
});

describe("GitLab's table of contents", () => {
  it('lists the headings where GitLab puts it', async () => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SECTIONS, forgeSource: GITLAB } });
    await waitFor(() => expect(container.querySelector('ul')).not.toBeNull());
    expect(container.querySelector('ul')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('Intro Usage Usage');
    expect(hrefs(container.querySelector('ul')!)).toEqual(['#intro', '#usage', '#usage-1']);
    expect(container.textContent).not.toContain('TOC');
  });

  it.each([['GitHub', GITHUB], ['agent chat', null]] as const)('stays text on %s', async (_name, forgeSource) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SECTIONS, forgeSource, embeddedHtml: forgeSource !== null } });
    await waitFor(() => expect(container.textContent).toContain('jump'));
    expect(container.querySelector('ul')).toBeNull();
    expect(container.textContent).toContain('[[');
  });
});

describe('both render paths', () => {
  it.each(['github', 'gitlab'] as const)('agree under a %s document context', async (forge: DocumentForge) => {
    const source = `${SECTIONS}\nRef[^n] and [def].\n\n[^n]: Note.\n\n[def]: https://example.invalid/def\n`;
    const extensions = EMBEDDED_HTML_EXTENSIONS;
    const blocks = createParseBlocksCache();
    parseBlocks(source, extensions, blocks);
    const document = documentContextOf(blocks, extensions, createDocumentContextCache(), forge);
    const { container } = render(CompactStaticMarkdownHarness, { source, extensions, document });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('Ref');
      expect(staticPath.textContent).toContain('Ref');
    });
    for (const root of [componentPath, staticPath]) {
      expect(anchors(root)).toEqual(['intro', 'usage', 'usage-1', 'named', ...(forge === 'github' ? ['ided'] : []), 'fnref:1']);
      expect(hrefs(root, 'a[data-md-fragment]')).toEqual([...(forge === 'gitlab' ? ['#intro', '#usage', '#usage-1'] : []), '#usage-1', '#named']);
      expect(hrefs(root, 'a:not([data-md-fragment])')).toEqual(['https://example.invalid/def']);
    }
    expect(componentPath.textContent).toBe(staticPath.textContent);
  });
});
