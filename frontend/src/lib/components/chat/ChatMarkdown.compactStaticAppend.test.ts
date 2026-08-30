import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';

function selectText(node: Text, start: number, end: number): Selection {
  const selection = window.getSelection();
  if (!selection) throw new Error('test browser has no Selection');
  selection.removeAllRanges();
  const range = document.createRange();
  range.setStart(node, start);
  range.setEnd(node, end);
  selection.addRange(range);
  return selection;
}

describe('compact completed block reconciliation', () => {
  it('appends without replacing prior DOM or its active selection', async () => {
    const first = 'First paragraph remains selected.';
    const second = 'Second paragraph remains mounted.';
    const view = render(ChatMarkdown, {
      props: { source: `${first}\n\n${second}` },
    });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(2));

    const [firstNode, secondNode] = Array.from(view.container.querySelectorAll('p'));
    const text = firstNode.firstChild;
    if (!(text instanceof Text)) throw new Error('first paragraph has no text node');
    const selection = selectText(text, 6, 15);
    expect(selection.toString()).toBe('paragraph');

    await view.rerender({
      source: `${first}\n\n${second}\n\nThird paragraph is appended.`,
    });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(3));

    const paragraphs = view.container.querySelectorAll('p');
    expect(paragraphs[0]).toBe(firstNode);
    expect(paragraphs[1]).toBe(secondNode);
    expect(selection.toString()).toBe('paragraph');
  });

  it('replaces only the changed suffix across rewrite, truncate, and regrow transitions', async () => {
    const first = 'Stable first paragraph.';
    const view = render(ChatMarkdown, {
      props: { source: `${first}\n\nOriginal second paragraph.` },
    });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(2));
    const stableNode = view.container.querySelector('p')!;
    const originalSecond = view.container.querySelectorAll('p')[1];

    await view.rerender({ source: `${first}\n\nRewritten second paragraph.` });
    await waitFor(() => expect(view.container.textContent).toContain('Rewritten second'));
    expect(view.container.querySelectorAll('p')[0]).toBe(stableNode);
    expect(view.container.querySelectorAll('p')[1]).not.toBe(originalSecond);

    await view.rerender({ source: first });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(1));
    expect(view.container.querySelector('p')).toBe(stableNode);

    await view.rerender({ source: `${first}\n\nRegrown second paragraph.` });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(2));
    expect(view.container.querySelectorAll('p')[0]).toBe(stableNode);
    expect(view.container.textContent).not.toContain('Rewritten second');
    expect(view.container.textContent).toContain('Regrown second');

    await view.rerender({ source: '' });
    await waitFor(() => expect(view.container.querySelector('.md-committed')).toBeNull());
    expect(view.container.textContent?.trim()).toBe('');
  });

  it('moves paragraph-gap metadata with an appended or rewritten sibling', async () => {
    const first = 'First paragraph.';
    const second = 'Second paragraph.';
    const view = render(ChatMarkdown, {
      props: { source: `${first}\n\n${second}` },
    });
    await waitFor(() => expect(view.container.querySelectorAll('p')).toHaveLength(2));
    let paragraphs = view.container.querySelectorAll('p');
    expect(paragraphs[0]).not.toHaveClass('sd-paragraph-gap');
    expect(paragraphs[1]).toHaveClass('sd-paragraph-gap');

    await view.rerender({ source: `${first}\n\n## Divider\n\n${second}` });
    await waitFor(() => expect(view.container.querySelector('h2')).not.toBeNull());
    paragraphs = view.container.querySelectorAll('p');
    expect(paragraphs[0]).not.toHaveClass('sd-paragraph-gap');
    expect(paragraphs[1]).not.toHaveClass('sd-paragraph-gap');

    await view.rerender({ source: `${first}\n\n${second}` });
    await waitFor(() => expect(view.container.querySelector('h2')).toBeNull());
    paragraphs = view.container.querySelectorAll('p');
    expect(paragraphs[1]).toHaveClass('sd-paragraph-gap');
  });

  it('keeps an unsupported component island mounted when static blocks append after it', async () => {
    const source = [
      'Before the fence.',
      '',
      '```',
      'const stable = true;',
      '```',
    ].join('\n');
    const view = render(ChatMarkdown, { props: { source } });
    await waitFor(() => expect(view.container.querySelector('.streamdown-code-host')).not.toBeNull());
    const codeHost = view.container.querySelector('.streamdown-code-host');

    await view.rerender({ source: `${source}\n\nAfter the fence.` });
    await waitFor(() => expect(view.container.textContent).toContain('After the fence.'));
    expect(view.container.querySelector('.streamdown-code-host')).toBe(codeHost);
    expect(view.container.querySelector('code')?.textContent).toBe('const stable = true;');

    view.unmount();
    expect(view.container.childNodes).toHaveLength(0);
  });

  it('settles a long stream by appending its tail without replacing committed DOM', async () => {
    const source = Array.from({ length: 40 }, (_, index) => [
      `### Working set ${index}`,
      '',
      `Paragraph ${index} stays mounted across final settle.`,
      '',
      '```',
      `const value${index} = ${index};`,
      '```',
      '',
    ].join('\n')).join('') + 'Final volatile paragraph.';
    const view = render(ChatMarkdown, {
      props: { source, streaming: true },
    });
    const committed = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('.md-committed');
      expect(found).not.toBeNull();
      expect(view.container.querySelector('.md-volatile')).not.toBeNull();
      return found!;
    });
    const firstHeading = committed.querySelector('h3');
    const firstCode = committed.querySelector('.streamdown-code-host');
    if (!firstHeading || !firstCode) throw new Error('committed prefix was incomplete');
    const committedHeadings = Array.from(committed.querySelectorAll('h3'));
    const committedCodeBlocks = Array.from(
      committed.querySelectorAll('.streamdown-code-host'),
    );

    await view.rerender({ source, streaming: false });
    await waitFor(() => expect(view.container.querySelector('.md-volatile')).toBeNull());

    expect(view.container.querySelector('.md-committed')).toBe(committed);
    expect(committed.querySelector('h3')).toBe(firstHeading);
    expect(committed.querySelector('.streamdown-code-host')).toBe(firstCode);
    expect(
      Array.from(committed.querySelectorAll('h3')).slice(0, committedHeadings.length),
    ).toEqual(committedHeadings);
    expect(
      Array.from(committed.querySelectorAll('.streamdown-code-host')).slice(
        0,
        committedCodeBlocks.length,
      ),
    ).toEqual(committedCodeBlocks);
    expect(committed.textContent).toContain('Final volatile paragraph.');
  });
});
