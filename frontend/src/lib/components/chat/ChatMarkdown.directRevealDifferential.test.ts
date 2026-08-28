import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import DirectMarkdownDifferentialHarness from './DirectMarkdownDifferentialHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

type Harness = {
  append(delta: string): Promise<boolean>;
  synchronize(): Promise<void>;
  remountDirect(): Promise<void>;
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
      name.startsWith('data-streamdown-') ? '' : value,
    ])
    .sort(([a], [b]) => a.localeCompare(b));
  const children: Array<NormalizedElement | string> = [];
  for (const child of root.childNodes) {
    if (child.nodeType === Node.TEXT_NODE) {
      const text = child.textContent ?? '';
      if (text !== '') {
        const previous = children.at(-1);
        if (typeof previous === 'string') {
          children[children.length - 1] = previous + text;
        } else {
          children.push(text);
        }
      }
    } else if (child.nodeType === Node.ELEMENT_NODE) {
      children.push(normalizeElement(child as Element));
    }
  }
  return {
    tag: root.tagName.toLowerCase(),
    attributes,
    children,
  };
}

function renderedPair(container: HTMLElement): [Element, Element] {
  const baseline = container.querySelector('[data-differential-baseline] > .markdown-body');
  const direct = container.querySelector('[data-differential-direct] > .markdown-body');
  if (!baseline || !direct) throw new Error('differential markdown roots did not mount');
  return [baseline, direct];
}

async function assertEveryAppend(
  source: string,
  chunks: readonly string[],
  options?: { pathRefs?: Array<{ path: string }>; workspacePath?: string },
): Promise<number> {
  const view = render(DirectMarkdownDifferentialHarness, {
    pathRefs: options?.pathRefs ?? [],
    workspacePath: options?.workspacePath ?? '',
  });
  const harness = view.component as unknown as Harness;
  let directAppends = 0;
  let consumed = '';
  for (const chunk of chunks) {
    consumed += chunk;
    if (await harness.append(chunk)) directAppends++;
    const [baseline, direct] = renderedPair(view.container);
    expect(normalizeElement(direct), `after ${JSON.stringify(consumed)}`).toEqual(
      normalizeElement(baseline),
    );
  }
  expect(consumed).toBe(source);
  await harness.synchronize();
  const [baseline, direct] = renderedPair(view.container);
  expect(normalizeElement(direct)).toEqual(normalizeElement(baseline));
  view.unmount();
  return directAppends;
}

const RICH_MARKDOWN = [
  '# Heading with streamed words',
  '',
  'Plain **strong text** and *emphasis* with ~~deleted words~~.',
  '',
  '> A quoted line with `inline code` and an entity &amp;.',
  '',
  '- first list item',
  '- second item with [a link](https://example.com/path?q=1)',
  '',
  '| Name | Value |',
  '| --- | ---: |',
  '| alpha | 42 |',
  '| beta | streamed cell text |',
  '',
  '```ts',
  'const answer = 42;',
  '```',
  '',
  '<span>raw html stays disabled</span>',
  '',
  'Unicode café 漢字 and combining e\u0301 stay exact.',
].join('\n');

beforeEach(() => {
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

describe('direct markdown reveal differential', () => {
  it('matches authoritative markdown after every character', async () => {
    const count = await assertEveryAppend(RICH_MARKDOWN, Array.from(RICH_MARKDOWN));
    expect(count).toBeGreaterThan(100);
  });

  it('matches provider-shaped split words, links, tables, and delimiters', async () => {
    const chunks = [
      '#', ' Heading ', 'with ', 'stream', 'ed ', 'words', '\n\n',
      'Plain ', '**', 'strong ', 'text', '**', ' and ', '*', 'emphasis', '*',
      ' with ', '~~', 'deleted ', 'words', '~~', '.', '\n\n',
      '> ', 'A ', 'quoted ', 'line ', 'with ', '`', 'inline ', 'code', '`',
      ' and ', 'an ', 'entity ', '&', 'amp', ';', '.', '\n\n',
      '- ', 'first ', 'list ', 'item', '\n', '- ', 'second ', 'item ', 'with ',
      '[', 'a ', 'link', '](', 'https', '://', 'example', '.', 'com', '/path', '?q=', '1', ')',
      '\n\n|', ' Name ', '|', ' Value ', '|\n|', ' --- ', '|', ' ---:', ' |\n|',
      ' alpha ', '|', ' 42 ', '|\n|', ' beta ', '|', ' streamed ', 'cell ', 'text ', '|',
      '\n\n```', 'ts', '\n', 'const ', 'answer ', '= ', '42', ';', '\n```',
      '\n\n<span', '>', 'raw ', 'html ', 'stays ', 'disabled', '</span>',
      '\n\nUnicode ', 'café ', '漢字 ', 'and ', 'combining ', 'e\u0301 ', 'stay ', 'exact', '.',
    ];
    const count = await assertEveryAppend(RICH_MARKDOWN, chunks);
    expect(count).toBeGreaterThan(15);
  });

  it('does not bypass path-link tokenization as an allowlisted path completes', async () => {
    const source = 'Open src/lib/main.ts:42 and continue.';
    const count = await assertEveryAppend(
      source,
      ['Open ', 'src', '/', 'lib', '/', 'main', '.', 'ts', ':', '42 ', 'and ', 'continue', '.'],
      {
        workspacePath: '/workspace',
        pathRefs: [{ path: 'src/lib/main.ts' }],
      },
    );
    expect(count).toBeGreaterThan(0);
  });

  it('falls back when a literal-only delta completes an allowlisted path', async () => {
    const source = 'Open README and continue.';
    const count = await assertEveryAppend(
      source,
      ['Open ', 'READ', 'ME ', 'and ', 'continue', '.'],
      {
        workspacePath: '/workspace',
        pathRefs: [{ path: 'README' }],
      },
    );
    expect(count).toBeGreaterThan(0);
  });

  it('restores the direct suffix when a visible representation remounts', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    await harness.append('The first ');
    await harness.append('authoritative words ');
    expect(await harness.append('then stream directly ')).toBe(true);
    expect(await harness.append('without reparsing ')).toBe(true);

    await harness.remountDirect();
    const [baseline, direct] = renderedPair(view.container);
    expect(normalizeElement(direct)).toEqual(normalizeElement(baseline));
  });
});
