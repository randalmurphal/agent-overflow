import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ThreadRowBadges from './ThreadRowBadges.svelte';
import type { Thread } from '../../types/models';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test Thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('ThreadRowBadges', () => {
  it('renders no trailing badge for terminal threads (the indicator moved to a leading ThreadRow icon)', () => {
    const { queryByLabelText } = render(ThreadRowBadges, {
      thread: makeThread({ mode: 'terminal' }),
    });
    // Terminal threads now show a LEADING green icon in ThreadRow
    // (thread-row-terminal-icon), not a trailing badge here.
    expect(queryByLabelText('Terminal Thread')).toBeNull();
  });

  it('renders the design badge (not the terminal one) for design threads', () => {
    const { getByLabelText, queryByLabelText } = render(ThreadRowBadges, {
      thread: makeThread({ mode: 'design' }),
    });
    expect(getByLabelText('Design Thread')).toBeTruthy();
    expect(queryByLabelText('Terminal Thread')).toBeNull();
  });

  it('renders no mode badge for a plain chat thread', () => {
    const { queryByLabelText } = render(ThreadRowBadges, {
      thread: makeThread({ mode: 'chat' }),
    });
    expect(queryByLabelText('Terminal Thread')).toBeNull();
    expect(queryByLabelText('Design Thread')).toBeNull();
  });
});
