import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import AssistantMessage from './AssistantMessage.svelte';

describe('<AssistantMessage>', () => {
  it('keeps one stable body wrapper for raw streaming text', async () => {
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
      expect(body.textContent).toContain('streaming **markdown');
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
      expect(updatedBody.innerHTML).toContain('<strong>markdown</strong>');
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
});
