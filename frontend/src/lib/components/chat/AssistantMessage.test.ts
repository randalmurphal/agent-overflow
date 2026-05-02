import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import AssistantMessage from './AssistantMessage.svelte';

describe('<AssistantMessage>', () => {
  it('renders incomplete markdown by auto-closing the dangling marker during stream', async () => {
    // Streamdown's `parseIncompleteMarkdown` flag (we set it from
    // `streaming`) auto-closes unterminated tokens so a partial bold
    // span (`**markdown`) renders bold immediately rather than
    // showing raw asterisks until the closer arrives. This gives a
    // smoother streaming UX than the legacy behaviour of waiting for
    // the closer.
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    expect(body.getAttribute('data-render-mode')).toBe('client-markdown');
    await waitFor(() => {
      // Auto-closed asterisks are gone; the text content reads as the
      // user-visible string, with `markdown` inside a strong element.
      expect(body.textContent).toContain('streaming markdown');
      expect(body.querySelector('strong')?.textContent).toBe('markdown');
    });
  });

  it('keeps the same body wrapper when completed markdown renders on the client', async () => {
    const { getByTestId, rerender } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
        }),
      },
    });

    const originalBody = getByTestId('assistant-message-body');

    await rerender({
      item: makeItem({
        status: 'completed',
        summary: 'streaming **markdown**',
      }),
    });

    const updatedBody = getByTestId('assistant-message-body');
    expect(updatedBody).toBe(originalBody);
    expect(updatedBody.getAttribute('data-render-mode')).toBe('client-markdown');
    await waitFor(() => {
      // Streamdown emits `<strong data-streamdown-strong=... class=...>`
      // — anchor on the wrapping element + its text rather than an
      // exact-string innerHTML match.
      const strong = updatedBody.querySelector('strong');
      expect(strong).not.toBeNull();
      expect(strong?.textContent).toBe('markdown');
    });
  });

  it('renders blank-line markdown as adjacent paragraph elements', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'first paragraph\n\nsecond paragraph',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // svelte-streamdown emits each markdown block via marked tokens
      // → a stable `[data-streamdown-paragraph]` element. We anchor on
      // that attribute instead of a positional `.markdown-body > p`
      // selector because Streamdown wraps its output in its own div.
      const paragraphs = [...body.querySelectorAll('p[data-streamdown-paragraph]')];
      expect(paragraphs.map((node) => node.textContent?.trim())).toEqual([
        'first paragraph',
        'second paragraph',
      ]);
    });
  });

  it('shows its timestamp without requiring row hover', () => {
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const { container } = render(AssistantMessage, {
      props: {
        item: makeItem({
          createdAt,
          summary: 'done',
        }),
      },
    });

    const time = container.querySelector('time');
    expect(time).not.toBeNull();
    expect(time?.getAttribute('datetime')).toBe(new Date(createdAt).toISOString());
    expect(time?.className).not.toContain('opacity-0');
    expect(time?.className).not.toContain('group-hover:opacity-100');
  });

  it('renders a copy button on a settled message with text', () => {
    const { getByLabelText } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary: 'done' }) },
    });
    expect(getByLabelText('Copy message')).toBeInTheDocument();
  });

  it('hides the copy button while the message is streaming', () => {
    const { container } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary: 'streaming text' }) },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('hides the copy button when summary is whitespace-only', () => {
    const { container } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary: '   \n  ' }) },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('writes the raw summary to the clipboard on click', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    const summary = '## Heading\n\n```ts\nconst x = 1;\n```';
    const { getByLabelText } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    await fireEvent.click(getByLabelText('Copy message'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(summary));
  });
});
