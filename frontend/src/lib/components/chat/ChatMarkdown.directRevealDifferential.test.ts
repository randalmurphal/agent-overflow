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

function normalizeStyle(value: string): string {
  return value
    .replace(/url\(#(?:ao-lucide|ao-mi)-\d+\)/g, 'url(#ICON)')
    .split(';')
    .map((declaration) => declaration.trim())
    .filter(Boolean)
    .map((declaration) => {
      const separator = declaration.indexOf(':');
      if (separator < 0) return declaration;
      return `${declaration.slice(0, separator).trim()}:${declaration.slice(separator + 1).trim()}`;
    })
    .join(';');
}

function normalizeElement(root: Element): NormalizedElement {
  const attributes = Array.from(root.attributes)
    // A retired code block replaces one Svelte click handler with the document
    // delegate. These markers express ownership, not rendered semantics.
    .filter(({ name }) =>
      name !== 'data-static-code-copy' &&
      name !== 'data-static-code-copy-icon'
    )
    .map(({ name, value }): [string, string] => [
      name,
      name.startsWith('data-streamdown-')
        ? ''
        : name === 'class'
          ? value.trim().replace(/\s+/g, ' ')
          : name === 'style'
            ? normalizeStyle(value)
            : value.replace(/((?:footnote|citation)-popover-)c\d+/g, '$1INSTANCE'),
    ])
    .sort(([a], [b]) => a.localeCompare(b));
  const children: Array<NormalizedElement | string> = [];
  const ignoresFormattingWhitespace = [
    'DIV', 'UL', 'OL', 'TABLE', 'THEAD', 'TBODY', 'TFOOT', 'TR', 'DL',
  ].includes(root.tagName);
  for (const child of root.childNodes) {
    if (child.nodeType === Node.TEXT_NODE) {
      const text = child.textContent ?? '';
      if (text !== '' && !(ignoresFormattingWhitespace && text.trim() === '')) {
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

const PARSER_TRANSITION_CORPUS = [
  'Bare URL growth: https://example.com/alpha?query=value and www.example.org/path.',
  'Bare email growth: reader@example.com and <other@example.org>.',
  [
    'A reference link [grows here][target].',
    '',
    '[target]: https://example.com/a-long-target "title words"',
  ].join('\n'),
  [
    'A paragraph before a definition.',
    '',
    '[unused]: https://example.com/definition-only',
  ].join('\n'),
  [
    '1. ordered item with words',
    '2. second item',
    '',
    '- [ ] pending task',
    '- [x] completed task',
  ].join('\n'),
  [
    '> [!NOTE]',
    '> Alert text keeps growing inside the custom container.',
    '>',
    '> - nested list text',
  ].join('\n'),
  [
    'Term',
    ': description text with **strong words**',
    '',
    'H~2~O and x^2^ custom inline containers.',
  ].join('\n'),
  [
    'Footnote text[^note] and inline math $alpha + beta$.',
    '',
    '[^note]: definition words continue here.',
  ].join('\n'),
  'Escapes \\*literal words\\* and entities &copy; &NotEqualTilde; remain exact.',
  'Unicode العربية עברית देवनागरी café e\u0301 漢字 😀 stays exact.',
  '<https://example.com/autolink> and `code words grow here`.',
  'Inline code normalizes `alpha  beta` without a direct-render divergence.',
  [
    '### ATX heading words ###',
    '',
    '---',
    '',
    'Paragraph with  ',
    'a hard break and a \\',
    'backslash break.',
  ].join('\n'),
  [
    '![blocked image](relative/image.png "image title") and ',
    '[titled link](https://example.com "link title").',
  ].join(''),
  [
    '~~~python',
    'print("tilde fence")',
    '~~~',
    '',
    '    indented code stays code',
  ].join('\n'),
  [
    '> outer quote',
    '>> nested quote with **words**',
    '>',
    '> final line',
  ].join('\n'),
  'Delimiter adjacency: alpha__beta__gamma, ~~strike words~~, and `tick ** text`.',
  'Citations [alpha, beta] [2] continue beside ordinary streamed words.',
  [
    '[center]',
    'Centered **streaming words** stay inside the alignment block.',
    '[/center]',
    '[right]',
    'Right aligned text keeps growing.',
    '[/right]',
  ].join('\n'),
  '<Widget label="demo">Nested *markdown words* inside MDX.</Widget>',
  'Explicit breaks use <br> and <br/> before more streamed words.',
  [
    '$$',
    '\\int_0^1 x^2 \\, dx',
    '$$',
    '',
    'Prose after display math keeps growing.',
  ].join('\n'),
];

function variableChunks(source: string, seed: number): string[] {
  const chunks: string[] = [];
  let offset = 0;
  let state = seed >>> 0;
  while (offset < source.length) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
    const length = 1 + (state % 19);
    chunks.push(source.slice(offset, offset + length));
    offset += length;
  }
  return chunks;
}

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

// Deterministic CPU sweeps over fixed corpora: the per-character
// transition sweep is ~3.2s on an idle core, and the default 5s budget
// flaked at ~1.6x suite contention (2026-08-30). The budget is a
// wedged-runtime tripwire, not a perf assertion — every lap is a bounded
// loop that cannot spin forever on its own.
describe('direct markdown reveal differential', { timeout: 60_000 }, () => {
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

  it('bypasses the parser for ordinary text inside an established table cell', async () => {
    const chunks = [
      '| Name | Value |\n| --- | --- |\n| row | streamed ',
      'cell ',
      'text ',
      '|\n',
    ];
    expect(await assertEveryAppend(chunks.join(''), chunks)).toBe(1);
  });

  it('matches authoritative output across parser transitions after every character', async () => {
    let directAppends = 0;
    for (const source of PARSER_TRANSITION_CORPUS) {
      directAppends += await assertEveryAppend(source, Array.from(source));
    }
    expect(directAppends).toBeGreaterThan(100);
  });

  it('matches authoritative output across mixed provider chunk boundaries', async () => {
    let directAppends = 0;
    for (let index = 0; index < PARSER_TRANSITION_CORPUS.length; index++) {
      const source = PARSER_TRANSITION_CORPUS[index];
      directAppends += await assertEveryAppend(
        source,
        variableChunks(source, 0x9e3779b9 ^ index),
      );
    }
    expect(directAppends).toBeGreaterThan(15);
  });

  it('matches authoritative output while common prose punctuation bypasses the parser', async () => {
    const chunks = [
      'Opening ',
      'sentence. ',
      'A ',
      'clause, ',
      'a ',
      'question? ',
      'An ',
      'answer! ',
      'A ',
      'label: ',
      "don't ",
      'change ',
      'markdown.',
    ];
    const source = chunks.join('');

    expect(await assertEveryAppend(source, chunks)).toBeGreaterThanOrEqual(6);
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
