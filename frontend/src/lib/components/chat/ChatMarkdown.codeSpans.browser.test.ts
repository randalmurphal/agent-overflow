import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { __resetStreamdownCodeHostForTest } from './markdown/StreamdownCodeHost.svelte';

const SOURCE = 'def route():\n    pass';

function keywordSpans() {
  return {
    lang: 'python',
    lines: [{ r: [3, 1] }, {}],
    truncated: false,
  };
}

function sourceText(root: Element): Text {
  const code = root.querySelector('code');
  if (!code) throw new Error('code element was not mounted');
  const walker = document.createTreeWalker(code, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (node instanceof Text && node.data.includes('route')) return node;
  }
  throw new Error('code source text was not mounted');
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
});

describe('completed code-island retirement', () => {
  it('keeps the copy control geometry and mask when the code island retires', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const view = render(ChatMarkdown, {
      props: { source: `\`\`\`python\n${SOURCE}\n\`\`\``, pathRefs: [] },
    });
    const liveButton = await waitFor(() => {
      const found = view.container.querySelector<HTMLButtonElement>(
        'button[aria-label="Copy code"]:not([data-static-code-copy])',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    const liveIcon = liveButton.querySelector<HTMLElement>('.lucide-copy');
    if (!liveIcon) throw new Error('live copy icon did not mount');
    const liveButtonRect = liveButton.getBoundingClientRect();
    const liveIconRect = liveIcon.getBoundingClientRect();

    try {
      resolveHighlight(keywordSpans());
      const staticButton = await waitFor(() => {
        const found = view.container.querySelector<HTMLButtonElement>(
          'button[data-static-code-copy]',
        );
        expect(found).not.toBeNull();
        return found!;
      });
      const staticIcon = staticButton.querySelector<HTMLElement>(
        '[data-static-code-copy-icon]',
      );
      if (!staticIcon) throw new Error('static copy icon did not mount');
      const staticButtonRect = staticButton.getBoundingClientRect();
      const staticIconRect = staticIcon.getBoundingClientRect();

      expect(staticButton.className.trim()).toBe(liveButton.className.trim());
      expect(staticButtonRect.width).toBeCloseTo(liveButtonRect.width, 3);
      expect(staticButtonRect.height).toBeCloseTo(liveButtonRect.height, 3);
      expect(staticIconRect.width).toBeCloseTo(liveIconRect.width, 3);
      expect(staticIconRect.height).toBeCloseTo(liveIconRect.height, 3);
      const maskImage = getComputedStyle(staticIcon).maskImage;
      expect(maskImage).toMatch(/#ao-mi-\d+/);
      const maskID = /#(ao-mi-\d+)/.exec(maskImage)?.[1];
      expect(maskID ? document.getElementById(maskID) : null).not.toBeNull();
    } finally {
      view.unmount();
    }
  });

  it('keeps selected text mounted until the selection clears', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const view = render(ChatMarkdown, {
      props: { source: `\`\`\`python\n${SOURCE}\n\`\`\``, pathRefs: [] },
    });
    const liveButton = await waitFor(() => {
      const found = view.container.querySelector<HTMLButtonElement>(
        'button[aria-label="Copy code"]:not([data-static-code-copy])',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    const text = sourceText(view.container);
    const selection = window.getSelection();
    if (!selection) throw new Error('browser has no Selection');
    selection.removeAllRanges();
    selection.setBaseAndExtent(text, 4, text, 9);
    document.dispatchEvent(new Event('selectionchange'));
    expect(selection.toString()).toBe('route');

    try {
      resolveHighlight(keywordSpans());
      await new Promise((resolve) => setTimeout(resolve, 0));
      await new Promise(requestAnimationFrame);
      expect(selection.toString()).toBe('route');
      expect(liveButton.isConnected).toBe(true);
      expect(view.container.querySelector('.syntax-keyword')).toBeNull();
      expect(view.container.querySelector('[data-static-code-copy]')).toBeNull();

      selection.removeAllRanges();
      document.dispatchEvent(new Event('selectionchange'));
      await waitFor(() => {
        expect(liveButton.isConnected).toBe(false);
        expect(view.container.querySelector('.syntax-keyword')).not.toBeNull();
        expect(view.container.querySelector('[data-static-code-copy]')).not.toBeNull();
      });
    } finally {
      selection.removeAllRanges();
      document.dispatchEvent(new Event('selectionchange'));
      view.unmount();
    }
  });

  it('keeps the focused copy control mounted until focus moves away', async () => {
    let resolveHighlight!: (value: ReturnType<typeof keywordSpans>) => void;
    setBindingMock(
      'HighlightCode',
      () => new Promise((resolve) => {
        resolveHighlight = resolve;
      }),
    );
    const view = render(ChatMarkdown, {
      props: { source: `\`\`\`python\n${SOURCE}\n\`\`\``, pathRefs: [] },
    });
    const liveButton = await waitFor(() => {
      const found = view.container.querySelector<HTMLButtonElement>(
        'button[aria-label="Copy code"]:not([data-static-code-copy])',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    liveButton.focus();
    expect(document.activeElement).toBe(liveButton);

    const outside = document.createElement('button');
    document.body.append(outside);
    try {
      resolveHighlight(keywordSpans());
      await waitFor(() => expect(view.container.querySelector('.syntax-keyword')).not.toBeNull());
      await new Promise(requestAnimationFrame);
      expect(document.activeElement).toBe(liveButton);
      expect(liveButton.isConnected).toBe(true);

      outside.focus();
      await waitFor(() => {
        expect(liveButton.isConnected).toBe(false);
        expect(view.container.querySelector('[data-static-code-copy]')).not.toBeNull();
      });
    } finally {
      outside.remove();
      view.unmount();
    }
  });
});
