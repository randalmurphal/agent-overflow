import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';

import ContextWindowMeter from './ContextWindowMeter.svelte';

describe('<ContextWindowMeter>', () => {
  it('displays usage against the full context window when compact limit is present', async () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 650,
          maxTokens: 2000,
          autoCompactPercent: 50,
          autoCompactTokenLimit: 1000,
        },
        thread: {
          id: 'thread-1',
          title: 'Thread',
          provider: 'codex',
          model: 'gpt-5.5',
          workspacePath: '/tmp',
          projectPath: '/tmp',
          mode: 'chat',
          reasoningEffort: 'medium',
          fastMode: false,
          contextWindow: 1050000,
          createdAt: 0,
          updatedAt: 0,
          archived: false,
        },
      },
    });

    await fireEvent.mouseEnter(getByLabelText(/Context Window/));

    expect(await screen.findByText('33% used')).toBeTruthy();
    expect(screen.getByText('650 / 2.0k tokens')).toBeTruthy();
    expect(screen.getByText('Compact at 50% (1.0k)')).toBeTruthy();
    expect(screen.getByLabelText('Context settings')).toBeTruthy();
  });

  it('falls back to the raw context window only when no compact limit is available', async () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 500,
          maxTokens: 2000,
        },
      },
    });

    await fireEvent.mouseEnter(getByLabelText(/Context Window/));

    expect(await screen.findByText('25% used')).toBeTruthy();
    expect(screen.getByText('500 / 2.0k tokens')).toBeTruthy();
  });

  it('does not display aggregate processed tokens in the context popover', async () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 126,
          maxTokens: 258400,
        },
      },
    });

    await fireEvent.mouseEnter(getByLabelText(/Context Window/));

    expect(await screen.findByText('126 / 258.4k tokens')).toBeTruthy();
    expect(screen.queryByText(/Total processed/i)).toBeNull();
    expect(screen.queryByText(/11.8k/)).toBeNull();
  });
});
