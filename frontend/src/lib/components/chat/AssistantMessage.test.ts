import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import AssistantMessage from './AssistantMessage.svelte';

describe('<AssistantMessage>', () => {
  it('keeps one stable body wrapper for raw streaming text', () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
          highlightedContent: '',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    expect(body.getAttribute('data-render-mode')).toBe('text');
    expect(body.className).toContain('markdown-body');
    expect(body.textContent).toContain('streaming **markdown');
  });

  it('keeps the same body wrapper when server-rendered markdown arrives', async () => {
    const { getByTestId, rerender } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
          highlightedContent: '',
        }),
      },
    });

    const originalBody = getByTestId('assistant-message-body');

    await rerender({
      item: makeItem({
        status: 'streaming',
        summary: 'streaming **markdown**',
        highlightedContent: '<p>streaming <strong>markdown</strong></p>',
      }),
    });

    const updatedBody = getByTestId('assistant-message-body');
    expect(updatedBody).toBe(originalBody);
    expect(updatedBody.getAttribute('data-render-mode')).toBe('html');
    expect(updatedBody.className).toContain('markdown-body');
    expect(updatedBody.innerHTML).toContain('<strong>markdown</strong>');
  });
});
