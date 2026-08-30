import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it } from 'vitest';
import { StreamingBoundarySplitter } from '../../../markdown/boundary';
import {
  createIncrementalLexCache,
  createProvenAppend,
  incrementalLex,
  parseBlocks,
  parseIncompleteMarkdown,
  Streamdown,
  type Extension,
  type ProvenAppend,
  type useStreamdown,
} from '../../../markdown';
import { chatMarkdownTheme } from './streamdownTheme';

type StreamdownContext = ReturnType<typeof useStreamdown>;

type StreamdownForensics = {
  readonly content: string;
  readonly blocks: readonly string[];
  readonly lastPath: string;
  readonly trailingBlock: { readonly kind: string } | null;
  readonly documentParseCalls: number;
  readonly documentPublications: number;
  readonly incrementalLexMetrics: {
    readonly calls: number;
  };
};

interface NormalizedElement {
  tag: string;
  attributes: Array<[string, string]>;
  children: Array<NormalizedElement | string>;
}

function normalizeElement(root: Element): NormalizedElement {
  const attributes = Array.from(root.attributes)
    .map(({ name, value }): [string, string] => [
      name,
      name.startsWith('data-streamdown-')
        ? ''
        : value.replace(/((?:footnote|citation)-popover-)c\d+/g, '$1INSTANCE'),
    ])
    .sort(([left], [right]) => left.localeCompare(right));
  const children: Array<NormalizedElement | string> = [];
  for (const child of root.childNodes) {
    if (child.nodeType === Node.TEXT_NODE) {
      const text = child.textContent ?? '';
      if (text === '') continue;
      const previous = children.at(-1);
      if (typeof previous === 'string') {
        children[children.length - 1] = previous + text;
      } else {
        children.push(text);
      }
    } else if (child.nodeType === Node.ELEMENT_NODE) {
      children.push(normalizeElement(child as Element));
    }
  }
  return { tag: root.tagName.toLowerCase(), attributes, children };
}

function forensics(container: HTMLElement): StreamdownForensics {
  const root = container.firstElementChild as (HTMLElement & {
    __aoStreamdownForensics?: StreamdownForensics;
  }) | null;
  if (!root?.__aoStreamdownForensics) {
    throw new Error('diagnostic Streamdown root did not mount');
  }
  return root.__aoStreamdownForensics;
}

function legacyVolatileTokens(source: string) {
  return parseBlocks(source).flatMap((block) =>
    incrementalLex(
      block,
      [],
      createIncrementalLexCache(),
      parseIncompleteMarkdown,
    ),
  );
}

function singleVolatileBlockTokens(source: string) {
  return incrementalLex(
    source,
    [],
    createIncrementalLexCache(),
    parseIncompleteMarkdown,
  );
}

type TokenModel = Record<string, unknown>;

function renderedTokenModel(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(renderedTokenModel);
  }
  if (!value || typeof value !== 'object') return value;

  const token = value as Record<string, unknown>;
  const model: TokenModel = {};
  for (const [key, child] of Object.entries(token)) {
    if (key === 'raw') continue;
    if (key === 'text' && Array.isArray(token.tokens)) continue;
    model[key] = renderedTokenModel(child);
  }
  return model;
}

function trimFirstLeaf(value: unknown): boolean {
  if (Array.isArray(value)) {
    for (const child of value) {
      if (trimFirstLeaf(child)) return true;
    }
    return false;
  }
  if (!value || typeof value !== 'object') return false;
  const model = value as TokenModel;
  if (typeof model.text === 'string') {
    model.text = model.text.trimStart();
    return true;
  }
  return trimFirstLeaf(model.tokens);
}

function trimLastLeaf(value: unknown): boolean {
  if (Array.isArray(value)) {
    for (let index = value.length - 1; index >= 0; index -= 1) {
      if (trimLastLeaf(value[index])) return true;
    }
    return false;
  }
  if (!value || typeof value !== 'object') return false;
  const model = value as TokenModel;
  if (typeof model.text === 'string') {
    model.text = model.text.trimEnd();
    return true;
  }
  return trimLastLeaf(model.tokens);
}

function renderedTokenStream(tokens: unknown[]): unknown[] {
  return tokens.map((token) => {
    const model = renderedTokenModel(token);
    const type = (model as TokenModel).type;
    if (type !== 'code') {
      trimFirstLeaf(model);
      trimLastLeaf(model);
    }
    return model;
  });
}

const MIXED_TAIL = [
  'Paragraph with **strong text**, `inline code`, and [a link](https://example.test).',
  '',
  '> Quoted text that remains in the conservative tail.',
  '',
  '- first list item',
  '- second list item',
  '',
  '| Name | Value |',
  '| --- | ---: |',
  '| alpha | 42 |',
  '',
  '```ts',
  'const first = true;',
  'const second = true;',
  '```',
  '',
  'A reference [resolved later][target].',
  '',
  '[target]: https://example.test/reference',
].join('\n');

describe('single volatile Streamdown block', () => {
  it('does not republish an unchanged committed block array', async () => {
    const source = 'Committed paragraph.\n\n- first item\n- second item';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: source,
        parseIncompleteMarkdown: false,
        compactStaticHtml: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();
    const state = forensics(view.container);
    expect(state.documentPublications).toBe(1);

    // A sibling-derived prop can invalidate Streamdown even though this
    // committed source did not move. The document parser is idempotent, and
    // its stable result must also stop before CompactBlocks reconciliation.
    await view.rerender({
      theme: chatMarkdownTheme,
      content: source,
      contentAppend: createProvenAppend('unrelated', '!'),
      parseIncompleteMarkdown: false,
      compactStaticHtml: true,
      diagnostics: true,
      get streamdown() {
        return context;
      },
      set streamdown(value: StreamdownContext | undefined) {
        context = value;
      },
    });
    await tick();

    expect(state.lastPath).toBe('unchanged');
    expect(state.documentParseCalls).toBe(2);
    expect(state.documentPublications).toBe(1);
  });

  it('renders the boundary-isolated tail without a document-level parse', async () => {
    const initial = 'Boundary-isolated paragraph';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: initial,
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();

    const state = forensics(view.container);
    expect(state.content).toEqualWithFirstDivergence(initial);
    expect(state.lastPath).toBe('initial-boundary');
    expect(state.trailingBlock?.kind).toBe('paragraph');
    expect(state.documentParseCalls).toBe(1);
    expect(state.incrementalLexMetrics.calls).toBeGreaterThan(0);
    expect(view.container.textContent).toEqualWithFirstDivergence(initial);

    const delta = ' keeps growing on its current line.';
    const append = createProvenAppend(initial, delta);
    await view.rerender({
      theme: chatMarkdownTheme,
      content: append.next,
      contentAppend: append,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();

    expect(state.lastPath).toBe('single-block');
    expect(state.documentParseCalls).toBe(1);
    expect(state.incrementalLexMetrics.calls).toBeGreaterThan(1);
    expect(view.container.textContent).toEqualWithFirstDivergence(append.next);

    await view.rerender({

      theme: chatMarkdownTheme,
      content: append.next,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: false,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).not.toBe('single-block');
    expect(state.documentParseCalls).toBeGreaterThan(0);
    const parsedCalls = state.documentParseCalls;

    await view.rerender({

      theme: chatMarkdownTheme,
      content: append.next,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).not.toBe('single-block');
    expect(state.documentParseCalls).toBe(parsedCalls + 1);

    const resumed = createProvenAppend(append.next, ' Still stable.');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: resumed.next,
      contentAppend: resumed,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).toBe('single-block');
    expect(state.documentParseCalls).toBe(parsedCalls + 1);
    expect(view.container.textContent).toEqualWithFirstDivergence(resumed.next);
  });

  it('returns to document parsing at a newline, then skips same-line fence growth', async () => {
    const initial = '```ts\nconst first = true;';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: initial,
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();
    const state = forensics(view.container);
    expect(state.documentParseCalls).toBe(1);

    const sameLine = createProvenAppend(initial, ' + 1');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: sameLine.next,
      contentAppend: sameLine,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).toBe('single-block');
    expect(state.documentParseCalls).toBe(1);

    const nextLine = createProvenAppend(sameLine.next, '\nconst second = true;');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: nextLine.next,
      contentAppend: nextLine,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).not.toBe('single-block');
    expect(state.documentParseCalls).toBe(2);

    const resumed = createProvenAppend(nextLine.next, ' // suffix');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: resumed.next,
      contentAppend: resumed,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();
    expect(state.lastPath).toBe('single-block');
    expect(state.documentParseCalls).toBe(2);
    // No host `components.code` here, so the fence renders through the
    // library's source-text fallback: one `<code>` holding the fence body.
    expect(view.container.querySelector('code')?.textContent?.split('\n')).toEqual([
      'const first = true; + 1',
      'const second = true; // suffix',
    ]);
  });

  it('rejects a fabricated same-line append proof', async () => {
    const initial = '```ts\nconst first = true;';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: initial,
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();
    const state = forensics(view.container);
    expect(state.documentParseCalls).toBe(1);

    const delta = ' forged';
    const next = initial + delta;
    const fabricated = { previous: initial, delta, next } as ProvenAppend;
    await view.rerender({
      theme: chatMarkdownTheme,
      content: next,
      contentAppend: fabricated,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();

    expect(state.lastPath).not.toBe('single-block');
    expect(state.documentParseCalls).toBe(2);
  });

  it('keeps parsing one-line shapes that can split before a newline', async () => {
    const initial = '| Name | ';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: initial,
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();
    const state = forensics(view.container);
    const append = createProvenAppend(initial, 'Value');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: append.next,
      contentAppend: append,
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();

    expect(state.lastPath).not.toBe('single-block');
    expect(state.documentParseCalls).toBe(2);
    expect(state.blocks).toEqual(['| Name |', ' Value']);
  });

  it('keeps custom block extensions authoritative across same-line appends', async () => {
    const customBlock: Extension = {
      name: 'split-alpha',
      level: 'block',
      applyInBlockParsing: true,
      tokenizer(source) {
        if (!source.startsWith('Alpha!')) return undefined;
        return { type: 'split-alpha', raw: 'Alpha!' };
      },
    };
    const initial = 'Alpha';
    let context: StreamdownContext | undefined;
    const view = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: initial,
        extensions: [customBlock],
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return context;
        },
        set streamdown(value: StreamdownContext | undefined) {
          context = value;
        },
      },
    });
    await tick();
    const state = forensics(view.container);
    const append = createProvenAppend(initial, '!Beta');
    await view.rerender({
      theme: chatMarkdownTheme,
      content: append.next,
      contentAppend: append,
      extensions: [customBlock],
      parseIncompleteMarkdown: true,
      isolatedVolatileTail: true,
      diagnostics: true,
    });
    await tick();

    // The custom extension still owns the tail parse. Reusing the identical
    // block-extension member across a fresh wrapper array must not invalidate
    // the sealed prefix, so the parser can take its normal append-tail path.
    expect(state.lastPath).toBe('append-tail');
    expect(state.documentParseCalls).toBe(2);
    expect(state.blocks).toEqual(['Alpha!', 'Beta']);
  });

  it('matches the former document-split token stream after every code unit', () => {
    const splitter = new StreamingBoundarySplitter();
    const mismatches: Array<{
      end: number;
      tail: string;
      single: unknown;
      legacy: unknown;
    }> = [];
    for (let end = 1; end <= MIXED_TAIL.length; end += 1) {
      const prefix = MIXED_TAIL.slice(0, end);
      const tail = splitter.split(prefix).tail;
      const single = renderedTokenStream(singleVolatileBlockTokens(tail));
      const legacy = renderedTokenStream(legacyVolatileTokens(tail));
      if (JSON.stringify(single) !== JSON.stringify(legacy)) {
        mismatches.push({ end, tail, single, legacy });
        if (mismatches.length === 3) break;
      }
    }
    expect(mismatches).toEqual([]);
  });

  it('keeps exact rendered DOM parity with the document-split path', async () => {
    let optimizedContext: StreamdownContext | undefined;
    let referenceContext: StreamdownContext | undefined;
    const optimized = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: '',
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: true,
        diagnostics: true,
        get streamdown() {
          return optimizedContext;
        },
        set streamdown(value: StreamdownContext | undefined) {
          optimizedContext = value;
        },
      },
    });
    const reference = render(Streamdown, {
      props: {
        theme: chatMarkdownTheme,
        content: '',
        parseIncompleteMarkdown: true,
        isolatedVolatileTail: false,
        diagnostics: true,
        get streamdown() {
          return referenceContext;
        },
        set streamdown(value: StreamdownContext | undefined) {
          referenceContext = value;
        },
      },
    });
    const splitter = new StreamingBoundarySplitter();

    for (let end = 1; end <= MIXED_TAIL.length; end += 1) {
      const source = MIXED_TAIL.slice(0, end);
      const split = splitter.split(source);
      const props = {
        theme: chatMarkdownTheme,
        content: split.tail,
        contentAppend: splitter.tailAppend,
        parseIncompleteMarkdown: true,
      };
      await optimized.rerender({ ...props, isolatedVolatileTail: true });
      await reference.rerender({ ...props, isolatedVolatileTail: false });
      await tick();

      const optimizedRoot = optimized.container.firstElementChild;
      const referenceRoot = reference.container.firstElementChild;
      if (!optimizedRoot || !referenceRoot) {
        throw new Error('differential Streamdown root did not mount');
      }
      const optimizedDom = normalizeElement(optimizedRoot);
      const referenceDom = normalizeElement(referenceRoot);
      expect(optimizedDom, `after ${JSON.stringify(source)}`).toEqual(
        referenceDom,
      );
    }
  }, 30_000);
});
