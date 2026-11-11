import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';
import { Lexer, Streamdown, lex } from '../../markdown';
import { renderedBlocks } from '../../utils/speakableText';
import { chatMarkdownTheme } from './markdown/streamdownTheme';
import { __setAlignmentEnabledForTest } from '../../markdown/parser/extensions/align';
import { getSettings } from '../../stores/settings.svelte';
import { serializeRangeToMarkdown } from '../../utils/markdownSerialize';
import { NOT_ALIGNMENT_CORPUS } from '../../../test/helpers/alignmentCorpus';

// `[center]` / `[right]` align every top-level block up to the closer, or
// to the end of the message, the same way settled and while streaming.

// Blocks join with no separator in textContent, so compare without spaces.
const text = (root: Element) => (root.textContent ?? '').replace(/\s+/g, '');

/** Each top-level rendered block: its alignment, element and text. */
const layout = (root: Element): string[] =>
  Array.from(root.querySelectorAll('.markdown-body > div > *')).map((el) => {
    const align = el.hasAttribute('data-streamdown-align')
      ? (el.classList.contains('text-right') ? 'right' : 'center')
      : '-';
    const element = align === '-' ? el : el.firstElementChild;
    return `${align}|${element?.tagName ?? ''}|${text(el)}`;
  });

// An opener line shows as text until its newline proves it a marker.
// The completer closes the bracket of a partial one: `[cen` shows `[cen]`.
const OPENER_PREFIX = /\[(?:c(?:e(?:n(?:t(?:e(?:r)?)?)?)?)?|r(?:i(?:g(?:h(?:t)?)?)?)?)?\]?$/;
const withoutOpenerPrefix = (shown: string) => shown.replace(OPENER_PREFIX, '');
// What later text must extend: no partial opener, and no bracket the
// completer closed provisionally.
const settledPart = (shown: string) => withoutOpenerPrefix(shown).replace(/\]$/, '');

const stable = (html: string) => html.replace(/(data-streamdown-[\w-]+)="[^"]*"/g, '$1');

/** Renders `source` streamed in `prefixes`, calling `each` after every one,
 * then settles it; returns the last streamed and the settled layout. */
async function stream(
  source: string,
  prefixes: string[],
  each: (container: HTMLElement, prefix: string) => void,
): Promise<{ streamed: string[]; settled: string[] }> {
  const view = render(ChatMarkdown, { props: { source: prefixes[0], streaming: true, pathRefs: [] } });
  for (const prefix of prefixes) {
    await view.rerender({ source: prefix, streaming: true, pathRefs: [] });
    await tick();
    each(view.container, prefix);
  }
  const streamed = layout(view.container);
  await view.rerender({ source, streaming: false, pathRefs: [] });
  await tick();
  const settled = layout(view.container);
  view.unmount();
  return { streamed, settled };
}

const everyPrefix = (source: string) => Array.from({ length: source.length }, (_, i) => source.slice(0, i + 1));

const random = (seed: number) => () => {
  seed = (seed * 1103515245 + 12345) & 0x7fffffff;
  return seed / 0x7fffffff;
};
const chunked = (source: string, seed: number): string[] => {
  const next = random(seed);
  const out: string[] = [];
  for (let end = 0; end < source.length;) {
    end = Math.min(source.length, end + 1 + Math.floor(next() * 8));
    out.push(source.slice(0, end));
  }
  return out;
};

const FAR = Array.from({ length: 12 }, (_, i) => `paragraph ${i}`).join('\n\n');

// [source, settled layout]
const STREAMED: [string, string[]][] = [
  ['[center]\nfirst paragraph\n\nsecond paragraph\n[/center]\n\nafter',
    ['center|P|firstparagraph', 'center|P|secondparagraph', '-|P|after']],
  ['[right]\n## Heading\n\n- one\n\n- two\n\nlast words\n[/right]\n\nafter',
    ['right|H2|Heading', 'right|UL|onetwo', 'right|P|lastwords', '-|P|after']],
  ['before\n\n[center]\nnever closed\n\n- a list\n- of two\n\nlast',
    ['-|P|before', 'center|P|neverclosed', 'center|UL|alistoftwo', 'center|P|last']],
  ['[center]\nforgot the right closer\n[/right]\n\nstill centered',
    ['center|P|forgottherightcloser[/right]', 'center|P|stillcentered']],
  [`[right]\n${FAR}\n[/right]\nafter`,
    [...Array.from({ length: 12 }, (_, i) => `right|P|paragraph${i}`), '-|P|after']],
  ['[center]\nA\n[/center]\n[right]\nB\n[/right]\nC',
    ['center|P|A', 'right|P|B', '-|P|C']],
];

// Tokens whose renderer emits several top-level children, or none.
const MULTI_CHILD = [
  '<Aside>\nOnly.\n</Aside>',
  '<Aside>\nFirst paragraph.\n\nSecond paragraph.\n</Aside>',
  '<Aside>\nInside.\n\n<Inner>\nDeep.\n</Inner>\n</Aside>',
  '<Aside>inline only</Aside>',
  'before\n\n<Aside>\n- a\n- b\n\n## heading\n\n```ts\nx\n```\n</Aside>\n\nafter',
  '# Title\n\n[ref]: https://example.com\n\n<div>raw</div>\n\n<!-- note -->\n\n> [!NOTE]\n> heads up\n\n$$\ne\n$$\n\n| a |\n|---|\n| 1 |\n\nDone[^1].\n\n[^1]: note',
];

/** The tag of each direct child, an aligned wrapper by the one element it holds. */
const childTags = (containers: Element[]): string[] =>
  containers.flatMap((container) => Array.from(container.children)).map((el) => {
    if (!el.hasAttribute('data-streamdown-align')) return el.tagName;
    expect(el.children, el.outerHTML).toHaveLength(1);
    return `aligned ${el.firstElementChild!.tagName}`;
  });
const unalignedTags = (tags: string[]) => tags.map((tag) => tag.replace(/^aligned /, ''));

describe('<ChatMarkdown> alignment', () => {
  describe.each(
    [...NOT_ALIGNMENT_CORPUS, ...MULTI_CHILD].flatMap((body) =>
      (['center', 'right'] as const)
        // A closer of the alignment would close it early; that is no longer the same body.
        .filter((align) => !new RegExp(`^\\[/${align}\\]`, 'm').test(body))
        .flatMap((align) => [
          [body, `[${align}]\n${body}\n[/${align}]`],
          [body, `[${align}]\n${body}`],
        ]),
    ),
  )('aligned %j as %j', (body, aligned) => {
    it('renders the same direct children, settled, as unaligned and as read aloud counts', async () => {
      const tags = async (source: string) => {
        const view = render(ChatMarkdown, { props: { source, pathRefs: [] } });
        await tick();
        const found = childTags(Array.from(view.container.querySelectorAll('.md-committed')));
        view.unmount();
        return found;
      };
      const plain = await tags(body);
      const shown = await tags(aligned);
      expect(unalignedTags(shown)).toEqual(plain);
      expect(shown).toHaveLength(renderedBlocks(lex(aligned)).length);
    });

    it('renders the same direct children live and static', async () => {
      for (const variant of [
        { parseIncompleteMarkdown: true },
        { static: true, compactStaticHtml: true },
        { static: true, compactStaticHtml: false },
      ]) {
        const tags = async (content: string) => {
          const view = render(Streamdown, { props: { content, theme: chatMarkdownTheme, ...variant } });
          await tick();
          const found = childTags([view.container.firstElementChild!]);
          view.unmount();
          return found;
        };
        expect(unalignedTags(await tags(aligned)), JSON.stringify(variant)).toEqual(await tags(body));
      }
    });
  });

  it('renders the same direct children around a forge HTML fragment', async () => {
    const tags = async (source: string) => {
      const view = render(ChatMarkdown, { props: { source, pathRefs: [], embeddedHtml: true } });
      await tick();
      const found = childTags(Array.from(view.container.querySelectorAll('.md-committed')));
      view.unmount();
      return found;
    };
    const body = 'lead\n\n<div>one</div>\n<p>two</p>\n\ntail';
    expect(unalignedTags(await tags(`[center]\n${body}`))).toEqual(await tags(body));
  });

  it.each([
    ['center', 'text-center'],
    ['right', 'text-right'],
  ])('renders each block of a settled [%s] block aligned', async (align, alignClass) => {
    const source = `before\n\n[${align}]\nAligned **bold** words\n\nsecond paragraph\n[/${align}]\n\nafter`;
    const { container } = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => expect(text(container)).toContain('after'));
    const blocks = container.querySelectorAll(`[data-streamdown-align].${alignClass}`);
    expect(blocks).toHaveLength(2);
    expect(blocks[0].querySelector('strong')?.textContent).toBe('bold');
    expect(text(container)).toBe('beforeAlignedboldwordssecondparagraphafter');
  });

  it('copies aligned blocks as the same markdown as unaligned ones', async () => {
    const copied = async (source: string) => {
      const { container, unmount } = render(ChatMarkdown, { props: { source, pathRefs: [] } });
      await waitFor(() => expect(text(container)).toContain('last'));
      const range = document.createRange();
      range.selectNodeContents(container.querySelector('.markdown-body')!);
      const markdown = serializeRangeToMarkdown(range);
      unmount();
      return markdown;
    };
    const body = 'first **bold**\n\n- one\n- two\n\n<Aside>\nFirst paragraph.\n\nSecond paragraph.\n</Aside>\n\n## heading\n\nlast';
    expect(await copied(`[center]\n${body}\n[/center]`)).toBe(await copied(body));
    expect(await copied(`[right]\n${body}`)).toBe(await copied(body));
  });

  it('wraps no block that renders nothing', async () => {
    const source = '[center]\nA\n\n[ref]: https://example.com\n\n<!-- note -->\n\nB';
    const { container } = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => expect(text(container)).toContain('B'));
    expect(layout(container)).toEqual(['center|P|A', 'center|P|B']);
  });

  describe.each([true, false])('streaming enabled: %s', (enabled) => {
    it.each(STREAMED)('streams %j without taking back text, settling to the same render', async (source, settled) => {
      const settings = getSettings();
      const saved = settings.streamingEnabled;
      settings.streamingEnabled = enabled;
      try {
        for (const prefixes of [everyPrefix(source), chunked(source, 7)]) {
          let previous = '';
          const result = await stream(source, prefixes, (container, prefix) => {
            const shown = text(container);
            const at = `at ${JSON.stringify(prefix)}: ${JSON.stringify({ previous, shown })}`;
            expect(shown.startsWith(settledPart(previous)), at).toBe(true);
            if (!source.includes('[/right]\n\nstill')) expect(withoutOpenerPrefix(shown), at).not.toContain('[');
            previous = shown;
          });
          if (enabled) expect(result.streamed).toEqual(settled);
          expect(result.settled).toEqual(settled);
        }
      } finally {
        settings.streamingEnabled = saved;
      }
    });
  });

  it('shows an open block aligned while it streams, before any closer', async () => {
    const source = '[center]\nfirst\n\n## second\n\n- third';
    await stream(source, everyPrefix(source).slice(9), (container, prefix) => {
      for (const line of layout(container)) expect(line, JSON.stringify(prefix)).toMatch(/^center\|/);
    });
  });

  it.each([...NOT_ALIGNMENT_CORPUS.map((doc) => [doc]), [NOT_ALIGNMENT_CORPUS.join('\n\n')]])(
    'renders text that is no marker exactly as with alignment off: %j',
    async (doc) => {
      for (const prefixes of [everyPrefix(doc), chunked(doc, 3)]) {
        const on = render(ChatMarkdown, { props: { source: prefixes[0], streaming: true, pathRefs: [] } });
        __setAlignmentEnabledForTest(false);
        const off = render(ChatMarkdown, { props: { source: prefixes[0], streaming: true, pathRefs: [] } });
        __setAlignmentEnabledForTest(true);
        try {
          const step = async (props: { source: string; streaming: boolean; pathRefs: never[] }) => {
            await on.rerender(props);
            await tick();
            const html = stable(on.container.innerHTML);
            __setAlignmentEnabledForTest(false);
            try {
              await off.rerender(props);
              await tick();
            } finally {
              __setAlignmentEnabledForTest(true);
            }
            expect(html, JSON.stringify(props)).toBe(stable(off.container.innerHTML));
          };
          for (const prefix of prefixes) await step({ source: prefix, streaming: true, pathRefs: [] });
          await step({ source: doc, streaming: false, pathRefs: [] });
        } finally {
          on.unmount();
          off.unmount();
        }
      }
    },
  );

  it.each([false, true])('follows a changed starting alignment over the same content (streaming: %s)', async (parseIncompleteMarkdown) => {
    // Streamdown's context and root are bindable props; a rerender must carry them.
    let context: unknown;
    let element: unknown;
    const props = (alignStart: 'center' | null) => ({
      content: 'A\n\nB',
      theme: chatMarkdownTheme,
      compactStaticHtml: true,
      parseIncompleteMarkdown,
      alignStart,
      get streamdown() { return context as never; },
      set streamdown(value) { context = value; },
      get element() { return element as never; },
      set element(value) { element = value; },
    });
    const view = render(Streamdown, { props: props(null) });
    const aligned = () => Array.from(view.container.querySelectorAll('[data-streamdown-align]')).map(text);
    await tick();
    expect(aligned()).toEqual([]);
    await view.rerender(props('center'));
    await tick();
    expect(aligned()).toEqual(['A', 'B']);
    await view.rerender(props(null));
    await tick();
    expect(aligned()).toEqual([]);
    expect(text(view.container)).toBe('AB');
  });

  it('lexes no more streaming inside an open block than outside one', async () => {
    const body = Array.from({ length: 40 }, (_, i) => `Paragraph ${i} of words that keep streaming in.`).join('\n\n');
    const work = async (source: string): Promise<number> => {
      let lexed = 0;
      const original = Lexer.prototype.blockTokens;
      const spy = vi.spyOn(Lexer.prototype, 'blockTokens').mockImplementation(function (this: Lexer, src, ...rest) {
        lexed += src.length;
        return original.call(this, src, ...(rest as [])) as never;
      });
      try {
        const words = source.split(/(?<= )/);
        let prefix = '';
        const view = render(ChatMarkdown, { props: { source: '', streaming: true, pathRefs: [] } });
        for (const word of words) {
          prefix += word;
          await view.rerender({ source: prefix, streaming: true, pathRefs: [] });
          await tick();
        }
        view.unmount();
      } finally {
        spy.mockRestore();
      }
      return lexed;
    };
    const plain = await work(`top\n\n${body}`);
    const aligned = await work(`[center]\n${body}`);
    expect(aligned).toBeLessThan(plain * 1.5);
  });
});
