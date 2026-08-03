import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';

import ContextWindowMeter from './ContextWindowMeter.svelte';
import type { Thread } from '../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

const claudeThread: Thread = {
  id: 'thread-claude',
  title: 'Thread',
  provider: 'claude',
  model: 'claude-fable-5',
  workspacePath: '/tmp',
  projectPath: '/tmp',
  mode: 'chat',
  reasoningEffort: 'medium',
  fastMode: false,
  contextWindow: 1000000,
  createdAt: 0,
  updatedAt: 0,
  archived: false,
};

describe('<ContextWindowMeter>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

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

    // The ring face shows the rounded percentage.
    expect(getByLabelText(/Context Window/).textContent?.trim()).toBe('33');

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
    // The ring face shows the MAX sentinel, not the raw percentage.
    expect(getByLabelText(/exceeded/).textContent?.trim()).toBe('MAX');
    await fireEvent.mouseEnter(getByLabelText(/Context Window/));
    expect(await screen.findByText('Context window exceeded')).toBeTruthy();
    // The "% used" line is replaced by the exceeded message.
    expect(screen.queryByText('100% used')).toBeNull();
  });

  // The upstream normalizer clamps non-finite values, but the display
  // must not depend on that: a normalizer bug would otherwise render a
  // literal "NaN" ring label and aria text.
  it('renders a non-finite wire percentage as 0, never a literal NaN', () => {
    const { getByLabelText } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 100,
          maxTokens: 2000,
          usedPercentage: NaN,
        },
      },
    });

    const button = getByLabelText(/Context Window/);
    expect(button.textContent?.trim()).toBe('0');
    expect(button.getAttribute('aria-label')).toContain('0% used');
  });

  // The meter forwards the raw wire percentage and relies on MeterRing
  // to clamp overshoot, so an out-of-range value must fill exactly one
  // revolution — never a negative dashoffset (longer-than-full arc).
  it('clamps an over-100 wire percentage to a full arc', () => {
    const { container } = render(ContextWindowMeter, {
      props: {
        data: {
          usedTokens: 300000,
          maxTokens: 200000,
          usedPercentage: 150,
        },
      },
    });

    const arc = container.querySelectorAll('svg circle')[1];
    expect(Number(arc.getAttribute('stroke-dashoffset'))).toBe(0);
  });

  // The exact breakdown costs a control round-trip to the live CLI, so
  // hovering must not trigger it — only the explicit click does.
  describe('exact breakdown affordance', () => {
    it('does not read the live session on hover alone', async () => {
      const mock = setBindingMock('GetThreadContextUsage', () => Promise.resolve({}));

      render(ContextWindowMeter, {
        props: {
          data: { usedTokens: 24028, maxTokens: 1000000, usedPercentage: 2.4 },
          thread: claudeThread,
        },
      });

      await fireEvent.mouseEnter(screen.getByLabelText(/Context Window/));
      expect(await screen.findByText('Show exact breakdown')).toBeTruthy();
      expect(mock).not.toHaveBeenCalled();
    });

    it('reads the live session when the user asks for it', async () => {
      const mock = setBindingMock('GetThreadContextUsage', () =>
        Promise.resolve({
          available: true,
          totalTokens: 24028,
          maxTokens: 1000000,
          percentage: 2,
          categories: [{ name: 'System prompt', tokens: 4027 }],
        }),
      );

      render(ContextWindowMeter, {
        props: {
          data: { usedTokens: 24028, maxTokens: 1000000, usedPercentage: 2.4 },
          thread: claudeThread,
        },
      });

      await fireEvent.mouseEnter(screen.getByLabelText(/Context Window/));
      await fireEvent.click(await screen.findByText('Show exact breakdown'));

      expect(await screen.findByText('System prompt')).toBeTruthy();
      expect(mock).toHaveBeenCalledTimes(1);
      expect(mock).toHaveBeenCalledWith('thread-claude');
    });

    // Codex reports a running token count with no categories, so offering
    // the affordance there would only ever produce an "unavailable" answer.
    it('is not offered on a Codex thread', async () => {
      render(ContextWindowMeter, {
        props: {
          data: { usedTokens: 650, maxTokens: 2000, usedPercentage: 32.5 },
          thread: { ...claudeThread, provider: 'codex', model: 'gpt-5.5' },
        },
      });

      await fireEvent.mouseEnter(screen.getByLabelText(/Context Window/));
      expect(await screen.findByText('33% used')).toBeTruthy();
      expect(screen.queryByText('Show exact breakdown')).toBeNull();
    });

    it('is not offered when the meter has no thread', async () => {
      render(ContextWindowMeter, {
        props: { data: { usedTokens: 500, maxTokens: 2000, usedPercentage: 25 } },
      });

      await fireEvent.mouseEnter(screen.getByLabelText(/Context Window/));
      expect(await screen.findByText('25% used')).toBeTruthy();
      expect(screen.queryByText('Show exact breakdown')).toBeNull();
    });

    // Closing the popover unmounts the breakdown — that is what keeps the
    // exact reading from outliving the moment it was taken. Reopening must
    // land back on the collapsed affordance, not on stale numbers.
    it('collapses again when the popover closes', async () => {
      setBindingMock('GetThreadContextUsage', () =>
        Promise.resolve({
          available: true,
          totalTokens: 24028,
          maxTokens: 1000000,
          percentage: 2,
          categories: [{ name: 'System prompt', tokens: 4027 }],
        }),
      );

      render(ContextWindowMeter, {
        props: {
          data: { usedTokens: 24028, maxTokens: 1000000, usedPercentage: 2.4 },
          thread: claudeThread,
        },
      });

      const button = screen.getByLabelText(/Context Window/);
      await fireEvent.mouseEnter(button);
      await fireEvent.click(await screen.findByText('Show exact breakdown'));
      expect(await screen.findByText('System prompt')).toBeTruthy();

      await fireEvent.mouseLeave(button);
      await new Promise((resolve) => setTimeout(resolve, 200));
      expect(screen.queryByText('System prompt')).toBeNull();

      await fireEvent.mouseEnter(button);
      expect(await screen.findByText('Show exact breakdown')).toBeTruthy();
    });
  });
});
