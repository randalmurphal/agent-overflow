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
          usedPercentage: 32.5,
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
    // Production data always reaches this component via
    // `normalizeContextWindowForThread`, which fills `usedPercentage`.
    // The fixture mirrors that contract.
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 500,
          maxTokens: 2000,
          usedPercentage: 25,
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
          usedPercentage: 0,
        },
      },
    });

    await fireEvent.mouseEnter(getByLabelText(/Context Window/));

    expect(await screen.findByText('126 / 258.4k tokens')).toBeTruthy();
    expect(screen.queryByText(/Total processed/i)).toBeNull();
    expect(screen.queryByText(/11.8k/)).toBeNull();
  });

  // The Go side computes UsedPercentage with a provider-aware formula
  // (Codex subtracts a 12000-token baseline). The meter must trust the
  // wire value rather than recomputing `usedTokens / maxTokens` — that
  // recomputation would silently undo the Codex baseline correction.
  it('trusts the wire usedPercentage over the local plain ratio', async () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 100000,
          maxTokens: 200000,
          usedPercentage: 46.81, // Codex baseline-aware value
        },
      },
    });

    await fireEvent.mouseEnter(getByLabelText(/Context Window/));

    // 47% (rounded), NOT 50% (the plain ratio).
    expect(await screen.findByText('47% used')).toBeTruthy();
    expect(screen.queryByText('50% used')).toBeNull();
  });

  it('renders the ContextWindowExceeded sentinel as a distinct state', async () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 200000,
          maxTokens: 200000,
          usedPercentage: 100,
          exceeded: true,
        },
      },
    });

    expect(getByLabelText(/exceeded/)).toBeTruthy();
    await fireEvent.mouseEnter(getByLabelText(/Context Window/));
    expect(await screen.findByText('Context window exceeded')).toBeTruthy();
    // The "% used" line is replaced by the exceeded message.
    expect(screen.queryByText('100% used')).toBeNull();
  });
});
