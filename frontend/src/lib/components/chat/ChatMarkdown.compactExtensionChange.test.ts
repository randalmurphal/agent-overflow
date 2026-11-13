import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { PATH_LINK_HREF_PREFIX, buildPathLinkExtension } from '../../utils/pathLinkExtension';
import type { Extension } from '../../markdown';

// Triage validates file paths while a message streams, and each newly
// validated path arrives as a new path-link extension. The compact renderer
// keeps the DOM of every finished block that extension tokenizes as before.

const SOURCE = [
  'First paragraph names src/foo.ts only.',
  'Second paragraph names src/bar.ts only.',
  'Third paragraph has [a link](https://example.test/x) and no path.',
  'Fourth paragraph is plain prose.',
].join('\n\n');

const PREFIXES = ['*', PATH_LINK_HREF_PREFIX];

function pathLinks(paths: string[], workspace = '/repo'): Extension[] {
  const extension = buildPathLinkExtension(paths.map((path) => ({ path })), workspace);
  if (!extension) throw new Error('extension must not be undefined');
  return [extension];
}

function compactParagraphs(container: Element): HTMLParagraphElement[] {
  return Array.from(container.querySelectorAll<HTMLParagraphElement>('[data-compact-static] p'));
}

function linkedPaths(root: Element, selector: string): string[] {
  return Array.from(root.querySelectorAll<HTMLAnchorElement>(`${selector} a[href^="agent-overflow:open"]`))
    .map((anchor) => anchor.textContent ?? '');
}

async function mounted(extensions: Extension[]) {
  const view = render(CompactStaticMarkdownHarness, {
    source: SOURCE,
    allowedLinkPrefixes: PREFIXES,
    extensions,
  });
  await waitFor(() => expect(compactParagraphs(view.container)).toHaveLength(4));
  return view;
}

describe('compact blocks across a path-link allowlist change', () => {
  it('rebuilds only the blocks a newly validated path can change', async () => {
    const view = await mounted(pathLinks(['src/foo.ts']));
    const before = compactParagraphs(view.container);
    expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts']);

    await view.rerender({ extensions: pathLinks(['src/foo.ts', 'src/bar.ts']) });
    await waitFor(() =>
      expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts', 'src/bar.ts']),
    );
    const after = compactParagraphs(view.container);
    expect(after).toHaveLength(4);
    expect(after[0]).toBe(before[0]);
    expect(after[1]).not.toBe(before[1]);
    // A markdown link's target is unescaped before the allowlist sees it,
    // so a block holding one is never kept.
    expect(after[2]).not.toBe(before[2]);
    expect(after[3]).toBe(before[3]);
    expect(before[1].isConnected).toBe(false);
    // Order survives an in-place rebuild, and both renderers agree.
    expect(after.map((paragraph) => paragraph.textContent)).toEqual(
      before.map((paragraph) => paragraph.textContent),
    );
    // The rebuilt paragraph keeps its adjacency spacing marker.
    expect(after.map((paragraph) => paragraph.className)).toEqual(
      before.map((paragraph) => paragraph.className),
    );
    expect(after[1].classList.contains('sd-paragraph-gap')).toBe(true);
    expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(
      linkedPaths(view.container, '[data-full-token-tree]'),
    );
  });

  it('unlinks a dropped path and keeps the rest across repeated changes', async () => {
    const view = await mounted(pathLinks(['src/foo.ts', 'src/bar.ts']));
    const first = compactParagraphs(view.container);

    await view.rerender({ extensions: pathLinks(['src/bar.ts']) });
    await waitFor(() =>
      expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/bar.ts']),
    );
    const second = compactParagraphs(view.container);
    expect(second[0]).not.toBe(first[0]);
    expect(second[1]).toBe(first[1]);
    expect(second[3]).toBe(first[3]);

    await view.rerender({ extensions: pathLinks(['src/foo.ts', 'src/bar.ts']) });
    await waitFor(() =>
      expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts', 'src/bar.ts']),
    );
    const third = compactParagraphs(view.container);
    expect(third[0]).not.toBe(second[0]);
    expect(third[1]).toBe(first[1]);
    expect(third[3]).toBe(first[3]);
    expect(third.map((paragraph) => paragraph.textContent)).toEqual(
      first.map((paragraph) => paragraph.textContent),
    );
  });

  it('rebuilds every block when the change cannot be scoped', async () => {
    const view = await mounted(pathLinks(['src/foo.ts']));
    const before = compactParagraphs(view.container);

    // A different workspace changes every rewritten href.
    await view.rerender({ extensions: pathLinks(['src/foo.ts'], '/elsewhere') });
    await waitFor(() => expect(compactParagraphs(view.container)[3]).not.toBe(before[3]));
    const moved = compactParagraphs(view.container);
    expect(moved.every((paragraph, index) => paragraph !== before[index])).toBe(true);

    // An extension with no answer invalidates everything, as does a list of
    // another length.
    const opaque: Extension = { name: 'opaque', level: 'inline', tokenizer: () => undefined };
    await view.rerender({ extensions: [opaque] });
    await waitFor(() => expect(compactParagraphs(view.container)[3]).not.toBe(moved[3]));
    const withOpaque = compactParagraphs(view.container);
    await view.rerender({ extensions: [{ ...opaque }] });
    await waitFor(() => expect(compactParagraphs(view.container)[3]).not.toBe(withOpaque[3]));
    const replaced = compactParagraphs(view.container);
    await view.rerender({ extensions: [...pathLinks(['src/foo.ts']), opaque] });
    await waitFor(() => expect(compactParagraphs(view.container)[3]).not.toBe(replaced[3]));
    expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts']);
  });
});

describe('a component-backed block across a path-link allowlist change', () => {
  // The code fence makes the whole quote a mounted Block, not static HTML.
  const QUOTED = [
    'Opening paragraph names src/foo.ts only.',
    '> The quote names src/bar.ts beside a fence.\n>\n> ```\n> code\n> ```',
    'Closing paragraph is plain prose.',
  ].join('\n\n');

  it('keeps the mounted block and lets it relink itself', async () => {
    const view = render(CompactStaticMarkdownHarness, {
      source: QUOTED,
      allowedLinkPrefixes: PREFIXES,
      extensions: pathLinks(['src/foo.ts']),
    });
    const compact = (selector: string) =>
      Array.from(view.container.querySelectorAll(`[data-compact-static] ${selector}`));
    await waitFor(() => expect(compact('blockquote')).toHaveLength(1));
    const quote = compact('blockquote')[0];
    const paragraphs = compact(':scope > * > p, :scope > p').filter((p) => !quote.contains(p));
    expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts']);

    await view.rerender({ extensions: pathLinks(['src/foo.ts', 'src/bar.ts']) });
    await waitFor(() =>
      expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(['src/foo.ts', 'src/bar.ts']),
    );
    expect(compact('blockquote')[0]).toBe(quote);
    expect(quote.querySelector('a[href^="agent-overflow:open"]')?.textContent).toBe('src/bar.ts');
    for (const paragraph of paragraphs) expect(paragraph.isConnected).toBe(true);
    expect(linkedPaths(view.container, '[data-compact-static]')).toEqual(
      linkedPaths(view.container, '[data-full-token-tree]'),
    );
  });
});

describe('ChatMarkdown across a growing pathRefs allowlist', () => {
  it('keeps the finished paragraphs the new path does not appear in', async () => {
    const props = { source: SOURCE, workspacePath: '/repo' };
    const view = render(ChatMarkdown, { props: { ...props, pathRefs: [{ path: 'src/foo.ts' }] } });
    const paragraphs = () => Array.from(view.container.querySelectorAll('p'));
    const linked = () =>
      Array.from(view.container.querySelectorAll('a[href^="agent-overflow:open"]'))
        .map((anchor) => anchor.textContent);
    await waitFor(() => expect(linked()).toEqual(['src/foo.ts']));
    const before = paragraphs();
    expect(before).toHaveLength(4);

    await view.rerender({ ...props, pathRefs: [{ path: 'src/foo.ts' }, { path: 'src/bar.ts' }] });
    await waitFor(() => expect(linked()).toEqual(['src/foo.ts', 'src/bar.ts']));
    const after = paragraphs();
    expect(after).toHaveLength(4);
    expect(after[0]).toBe(before[0]);
    expect(after[1]).not.toBe(before[1]);
    expect(after[3]).toBe(before[3]);
  });

  it('keeps them while the message is still streaming', async () => {
    // The trailing paragraph is the volatile tail; the rest is committed.
    const props = { source: `${SOURCE}\n\nStill being written`, workspacePath: '/repo', streaming: true };
    const view = render(ChatMarkdown, { props: { ...props, pathRefs: [{ path: 'src/foo.ts' }] } });
    const committed = () => Array.from(view.container.querySelectorAll('p')).slice(0, 4);
    const linked = () =>
      Array.from(view.container.querySelectorAll('a[href^="agent-overflow:open"]'))
        .map((anchor) => anchor.textContent);
    await waitFor(() => expect(linked()).toEqual(['src/foo.ts']));
    const before = committed();
    expect(before).toHaveLength(4);

    await view.rerender({ ...props, pathRefs: [{ path: 'src/foo.ts' }, { path: 'src/bar.ts' }] });
    await waitFor(() => expect(linked()).toEqual(['src/foo.ts', 'src/bar.ts']));
    const after = committed();
    expect(after[0]).toBe(before[0]);
    expect(after[1]).not.toBe(before[1]);
    expect(after[3]).toBe(before[3]);
  });
});
