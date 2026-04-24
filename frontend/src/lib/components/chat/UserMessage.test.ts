import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import UserMessage from './UserMessage.svelte';

describe('<UserMessage>', () => {
  it('shows its timestamp without requiring row hover', () => {
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const { container } = render(UserMessage, {
      props: {
        item: makeItem({
          createdAt,
          kind: 'user_text',
          role: 'user',
          summary: 'hello',
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
