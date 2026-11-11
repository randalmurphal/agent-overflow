import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';

// `[center]` / `[right]` blocks render their content aligned as written,
// settled and while streaming.

// Blocks join with no separator in textContent, so compare without spaces.
const text = (root: Element) => (root.textContent ?? '').replace(/\s+/g, '');

describe('<ChatMarkdown> alignment blocks', () => {
  it.each([
    ['center', 'text-center'],
    ['right', 'text-right'],
  ])('renders a settled [%s] block aligned with its content', async (align, alignClass) => {
    const source = `before\n\n[${align}]\nAligned **bold** words\n\nsecond paragraph\n[/${align}]\n\nafter`;
    const { container } = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => expect(text(container)).toContain('after'));
    const block = container.querySelector(`[data-streamdown-align].${alignClass}`);
    expect(block).not.toBeNull();
    expect(block!.querySelector('strong')?.textContent).toBe('bold');
    expect(block!.querySelectorAll('p')).toHaveLength(2);
    expect(text(container)).toBe('beforeAlignedboldwordssecondparagraphafter');
  });

  it.each([
    ['[center]\nfirst paragraph\n\nsecond paragraph\n[/center]\n\nafter', 'firstparagraphsecondparagraphafter'],
    ['[right]\n## Heading\n\n- one\n\n- two\n\nlast words\n[/right]\n\nafter', 'Headingonetwolastwordsafter'],
  ])('shows an open block while it streams and never takes back text when it closes: %j', async (source, settled) => {
    const start = source.indexOf('\n') + 2;
    const view = render(ChatMarkdown, { props: { source: source.slice(0, start), streaming: true, pathRefs: [] } });
    let previous = '';
    for (let end = start; end <= source.length; end++) {
      await view.rerender({ source: source.slice(0, end), streaming: true, pathRefs: [] });
      await tick();
      const shown = text(view.container);
      expect(shown, `at ${JSON.stringify(source.slice(0, end))}`).not.toContain('[');
      expect(shown.startsWith(previous), `at ${JSON.stringify(source.slice(0, end))}: ${JSON.stringify({ previous, shown })}`).toBe(true);
      previous = shown;
      // The open block is one aligned element holding all of its text.
      if (end <= source.indexOf('after')) {
        expect(view.container.querySelectorAll('[data-streamdown-align]'), `at ${JSON.stringify(source.slice(0, end))}`).toHaveLength(1);
        expect(text(view.container.querySelector('[data-streamdown-align]')!)).toBe(shown);
      }
    }
    await view.rerender({ source, streaming: false, pathRefs: [] });
    await tick();
    expect(text(view.container)).toBe(previous);
    expect(previous).toBe(settled);
  });
});
