import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';

interface NormalizedElement {
  tag: string;
  attributes: Array<[string, string]>;
  children: Array<NormalizedElement | string>;
}

function normalizeElement(root: Element): NormalizedElement {
  const attributes = Array.from(root.attributes)
    .filter(({ name }) => !(root instanceof HTMLInputElement && name === 'checked'))
    .map(({ name, value }): [string, string] => [
      name,
      name.startsWith('data-streamdown-') ? '' : value,
    ])
    .sort(([left], [right]) => left.localeCompare(right));
  const children: Array<NormalizedElement | string> = [];
  const ignoresFormattingWhitespace = [
    'DIV', 'UL', 'OL', 'TABLE', 'THEAD', 'TBODY', 'TFOOT', 'TR', 'DL',
  ].includes(root.tagName);
  for (const child of root.childNodes) {
    if (child.nodeType === Node.TEXT_NODE) {
      const text = child.textContent ?? '';
      if (text === '' || (ignoresFormattingWhitespace && text.trim() === '')) continue;
      const previous = children.at(-1);
      if (typeof previous === 'string') children[children.length - 1] = previous + text;
      else children.push(text);
    } else if (child.nodeType === Node.ELEMENT_NODE) {
      children.push(normalizeElement(child as Element));
    }
  }
  return { tag: root.tagName.toLowerCase(), attributes, children };
}

function roots(container: HTMLElement): [Element, Element] {
  const full = container.querySelector('[data-full-token-tree] > div');
  const compact = container.querySelector('[data-compact-static] > div');
  if (!full || !compact) throw new Error('differential Streamdown roots did not mount');
  return [full, compact];
}

function nonElementNodeCount(root: Element): number {
  const walker = document.createTreeWalker(
    root,
    NodeFilter.SHOW_TEXT | NodeFilter.SHOW_COMMENT,
  );
  let count = 0;
  while (walker.nextNode()) count++;
  return count;
}

const COMMON_MARKDOWN = [
  '# Heading 1',
  '',
  '## Heading 2',
  '',
  '### Heading 3 with **strong**, *emphasis*, ~~deleted~~, H~2~O, and x^2^',
  '',
  'Literal <tag> text is escaped, while `a < b && c > d` stays code.',
  '',
  '[Allowed title](https://example.com/path?q=1 "Link title") and [relative](docs/guide.md).',
  '',
  '- plain item',
  '- [ ] pending task',
  '- [x] completed task',
  '  1. nested ordered item',
  '  2. second nested item',
  '',
  '| Left | Center | Right |',
  '| :--- | :---: | ---: |',
  '| alpha | **beta** | `gamma` |',
  '| one | two | three |',
  '',
  '> A quote with **formatting**.',
  '>',
  '> - nested quote item',
  '',
  'Term',
  ': description with *formatted text*',
  '',
  '---',
  '',
  'A hard break follows.  ',
  'Next line and Unicode café 東京 🧪 e\u0301.',
].join('\n');

const FALLBACK_MARKDOWN = [
  '```ts',
  'const escaped = "<script>";',
  '```',
  '',
  '> [!NOTE]',
  '> Component-backed alert content.',
  '',
  '![remote image](https://example.com/image.png "Image title")',
].join('\n');

describe('compact completed Markdown rendering', () => {
  it.each([
    ['fixed synchronous tokens', COMMON_MARKDOWN],
    ['component-backed fallback tokens', FALLBACK_MARKDOWN],
  ])('matches the full token tree for %s', async (_name, source) => {
    const { container } = render(CompactStaticMarkdownHarness, { source });

    await waitFor(() => {
      const [, compact] = roots(container);
      expect(compact.textContent).toContain(source === COMMON_MARKDOWN ? 'Unicode café' : 'Component-backed alert');
    });

    const [full, compact] = roots(container);
    expect(normalizeElement(compact)).toEqual(normalizeElement(full));
    expect(
      Array.from(compact.querySelectorAll<HTMLInputElement>('input'), (input) => input.checked),
    ).toEqual(
      Array.from(full.querySelectorAll<HTMLInputElement>('input'), (input) => input.checked),
    );
    if (source === COMMON_MARKDOWN) {
      for (const root of [full, compact]) {
        const tasks = Array.from(root.querySelectorAll('input[type="checkbox"]'));
        expect(tasks).not.toHaveLength(0);
        for (const task of tasks) {
          expect(task.parentElement).toHaveClass('md-task-list-item');
        }
      }
    }
  });

  it('removes the settled per-token control-node multiplier', async () => {
    const source = Array.from({ length: 12 }, () => COMMON_MARKDOWN).join('\n\n');
    const { container } = render(CompactStaticMarkdownHarness, { source });
    const [full, compact] = roots(container);

    await waitFor(() => expect(compact.textContent).toContain('Unicode café'));

    expect(nonElementNodeCount(compact)).toBeLessThan(nonElementNodeCount(full) / 4);
  });

  it('never turns model-authored HTML into live elements', async () => {
    const source = [
      '<script data-injected>globalThis.injected = true</script>',
      '',
      '<img data-injected src=x onerror="globalThis.injected = true">',
      '',
      'Escaped prose: &lt;b data-injected&gt;safe&lt;/b&gt;.',
    ].join('\n');
    const { container } = render(CompactStaticMarkdownHarness, { source });
    const [, compact] = roots(container);

    await waitFor(() => expect(compact.textContent).toContain('Escaped prose'));
    expect(compact.querySelector('[data-injected]')).toBeNull();
    expect(compact.textContent).toContain('&lt;b data-injected&gt;safe&lt;/b&gt;');
  });
});
