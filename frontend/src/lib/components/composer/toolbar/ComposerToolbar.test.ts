import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import ComposerToolbar from './ComposerToolbar.svelte';
import { createThreadPane, type ThreadPane } from '../../../stores/thread.svelte';
import {
  resetForTest as resetProviderAccounts,
  setProviderAccount,
} from '../../../stores/accountInfo.svelte';
import { resetBindingMocks } from '../../../../test/mocks/bindings-app';
import type { Thread } from '../../../types/models';
import type { SendButtonAction } from './sendButtonTypes';

interface ToolbarTestProps {
  pane: ThreadPane;
  canSend: boolean;
  isTurnActive: boolean;
  sendLabel: string;
  sendAction: SendButtonAction;
  hasCurrentPlan: boolean;
  onSend: () => void;
  onInterrupt: () => void;
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'plan',
    model: 'claude-opus-4-7',
    reasoningEffort: 'xhigh',
    contextWindow: 1000000,
    runtimeMode: 'full-access',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function toolbarProps(
  pane = (() => {
    const p = createThreadPane();
    p.replaceThread(makeThread());
    return p;
  })(),
  overrides: Partial<ToolbarTestProps> = {},
): ToolbarTestProps {
  return {
    pane,
    canSend: true,
    isTurnActive: false,
    sendLabel: 'Implement',
    sendAction: 'implement' as const,
    hasCurrentPlan: true,
    onSend: () => {},
    onInterrupt: () => {},
    ...overrides,
  };
}

function renderToolbar(thread: Thread = makeThread()) {
  const pane = createThreadPane();
  pane.replaceThread(thread);
  return render(ComposerToolbar, { props: toolbarProps(pane) });
}

function installToolbarDimensions(
  requiredFullWidth: number | (() => number),
  availableWidth = 520,
  // jsdom applies no CSS, so the width a denser rung would free is
  // scripted here: hiding the collapsible labels saves this many pixels,
  // mirroring the real coupling where data-density changes scrollWidth.
  compactSavings = 140,
) {
  const clientSpy = vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get')
    .mockImplementation(function clientWidth(this: HTMLElement) {
      return this.dataset.testid === 'composer-toolbar' ? availableWidth : 0;
    });
  const scrollSpy = vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get')
    .mockImplementation(function scrollWidth(this: HTMLElement) {
      const width = typeof requiredFullWidth === 'function'
        ? requiredFullWidth()
        : requiredFullWidth;
      if (this.dataset.testid !== 'composer-toolbar') return 0;
      return this.dataset.density === 'full' || this.dataset.density === undefined
        ? width
        : width - compactSavings;
    });

  return () => {
    clientSpy.mockRestore();
    scrollSpy.mockRestore();
  };
}

describe('<ComposerToolbar>', () => {
  let restoreAnimationFrame: (() => void) | undefined;
  let restoreDimensions: (() => void) | undefined;

  beforeEach(() => {
    resetBindingMocks();
    resetProviderAccounts();
    const frame = vi.spyOn(window, 'requestAnimationFrame')
      .mockImplementation((callback: FrameRequestCallback) => {
        callback(0);
        return 0;
      });
    const cancel = vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {});
    restoreAnimationFrame = () => {
      frame.mockRestore();
      cancel.mockRestore();
    };
  });

  afterEach(() => {
    resetProviderAccounts();
    restoreDimensions?.();
    restoreDimensions = undefined;
    restoreAnimationFrame?.();
    restoreAnimationFrame = undefined;
  });

  it('compacts collapsible toolbar labels when full contents overflow', async () => {
    restoreDimensions = installToolbarDimensions(640);
    const { getByTestId, getAllByText } = renderToolbar();

    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'compact');
    });
    expect(getAllByText('Plan').some((el) =>
      el.getAttribute('data-composer-toolbar-label') === 'collapsible',
    )).toBe(true);
    const collapsiblePlan = getAllByText('Plan').find((el) =>
      el.getAttribute('data-composer-toolbar-label') === 'collapsible',
    );
    expect(collapsiblePlan).toBeTruthy();
  });

  it('drops to the minimal rung when even icon-only contents overflow', async () => {
    restoreDimensions = installToolbarDimensions(800);
    const { getByTestId } = renderToolbar();

    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'minimal');
    });
  });

  it('the minimal rung keeps the model and the meters and rolls the other pickers up', async () => {
    // The model and the meters are what a phone user reads before sending;
    // the other pickers are one tap further away, not gone. Everything is
    // mounted at every rung (the rung is CSS), so the assertion is that
    // the roll-up exists, that it carries no model row (the model stays a
    // control), and that its row opens the picker the chord would —
    // through the same registry handle.
    restoreDimensions = installToolbarDimensions(800);
    // The suite's synchronous rAF stub is for the density measurement; an
    // open Popover re-requests a frame per frame to follow its anchor,
    // which a synchronous stub turns into unbounded recursion.
    vi.mocked(window.requestAnimationFrame).mockImplementation((callback) => {
      setTimeout(() => callback(performance.now()), 0);
      return 0;
    });
    const { container, getByTestId, getByRole, queryByRole } = renderToolbar();
    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'minimal');
    });
    expect(container.querySelectorAll('[data-composer-toolbar-meter]').length).toBeGreaterThan(0);
    // The model trigger is outside the picker box the rung hides.
    const modelTrigger = getByTestId('composer-model-menu-trigger');
    expect(modelTrigger.closest('[data-composer-toolbar-pickers]')).toBeNull();
    const effortTrigger = getByTestId('composer-effort-trigger');
    expect(effortTrigger.closest('[data-composer-toolbar-pickers]')).not.toBeNull();
    expect(effortTrigger).toHaveAttribute('aria-expanded', 'false');

    await fireEvent.click(getByTestId('composer-pickers-rollup'));
    expect(queryByRole('menuitem', { name: 'Model…' })).toBeNull();
    await fireEvent.click(getByRole('menuitem', { name: 'Effort…' }));
    await waitFor(() => {
      expect(effortTrigger).toHaveAttribute('aria-expanded', 'true');
    });
  });

  it('keeps full toolbar labels when full contents fit', async () => {
    restoreDimensions = installToolbarDimensions(500);
    const { getByTestId, getAllByText } = renderToolbar();

    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'full');
    });
    const collapsiblePlan = getAllByText('Plan').find((el) =>
      el.getAttribute('data-composer-toolbar-label') === 'collapsible',
    );
    expect(collapsiblePlan).toBeTruthy();
  });

  it('renders the runtime-access, MCP, and plan toggles for a claude thread', () => {
    const { queryByTestId } = renderToolbar(makeThread({ provider: 'claude' }));
    expect(queryByTestId('composer-access-toggle')).not.toBeNull();
    expect(queryByTestId('composer-mcp-trigger')).not.toBeNull();
    expect(queryByTestId('composer-agent-mode-toggle')).not.toBeNull();
  });

  it('omits the unsupported toggles for a claude-tui thread', () => {
    // claude-tui has no AO-mediated runtime-mode / MCP / plan affordances —
    // they live inside the real TUI, reached via take-control. The composer
    // toolbar must not render those controls at all.
    const { queryByTestId } = renderToolbar(makeThread({ provider: 'claude-tui' }));
    expect(queryByTestId('composer-access-toggle')).toBeNull();
    expect(queryByTestId('composer-mcp-trigger')).toBeNull();
    expect(queryByTestId('composer-agent-mode-toggle')).toBeNull();
    // The model + effort pickers stay — claude-tui reuses claude's catalog.
    expect(queryByTestId('composer-effort-trigger')).not.toBeNull();
  });

  it('keeps the account serving the live session out of the toolbar label', () => {
    setProviderAccount(
      'codex',
      { email: 'new@example.com', subscriptionType: 'pro' },
      'new-account',
    );
    const pane = createThreadPane();
    pane.replaceThread(makeThread({ provider: 'codex', model: 'gpt-5.3-codex' }));
    pane.upsertItems([{
      id: 'user-1',
      threadId: 'thread-1',
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'Hello',
      createdAt: 1,
      updatedAt: 1,
    }]);
    pane.setProviderSessionAccount({
      threadId: 'thread-1',
      provider: 'codex',
      accountId: 'old-account',
      account: { email: 'old@example.com', subscriptionType: 'plus' },
      connected: true,
    });

    const { queryByTestId, queryByText } = render(ComposerToolbar, {
      props: toolbarProps(pane),
    });
    expect(queryByTestId('composer-provider-account')).toBeNull();
    expect(queryByText('old@example.com')).toBeNull();
    expect(queryByText('new@example.com')).toBeNull();
  });

  it('does not expose selected-account metadata beside the usage rings', () => {
    setProviderAccount(
      'codex',
      { email: 'fresh@example.com', subscriptionType: 'pro' },
      'same-account',
    );
    const pane = createThreadPane();
    pane.replaceThread(makeThread({ provider: 'codex', model: 'gpt-5.3-codex' }));
    pane.upsertItems([{
      id: 'user-1',
      threadId: 'thread-1',
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'Hello',
      createdAt: 1,
      updatedAt: 1,
    }]);
    pane.setProviderSessionAccount({
      threadId: 'thread-1',
      provider: 'codex',
      accountId: 'same-account',
      account: { email: 'stale@example.com', subscriptionType: 'plus' },
      connected: true,
    });

    const { queryByTestId, queryByText } = render(ComposerToolbar, {
      props: toolbarProps(pane),
    });
    expect(queryByTestId('composer-provider-account')).toBeNull();
    expect(queryByText('fresh@example.com')).toBeNull();
  });

  it('remeasures on a child width delivery without a container resize', async () => {
    // happy-dom's stub ResizeObserver never delivers; capture the
    // callbacks and deliver by hand. The width trigger is the RO on the
    // toolbar's direct children — a control whose rendered width moves
    // (the send label flipping, the context meter growing a digit)
    // delivers post-layout, and a text beat that moves no width delivers
    // nothing (the old subtree MutationObserver re-measured per beat).
    restoreAnimationFrame?.();
    restoreAnimationFrame = undefined;
    const queuedFrames: FrameRequestCallback[] = [];
    let nextFrame = 1;
    const frame = vi.spyOn(window, 'requestAnimationFrame')
      .mockImplementation((callback: FrameRequestCallback) => {
        queuedFrames.push(callback);
        return nextFrame++;
      });
    const cancel = vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {});
    restoreAnimationFrame = () => {
      frame.mockRestore();
      cancel.mockRestore();
    };

    const runNextFrame = () => {
      const callback = queuedFrames.shift();
      if (!callback) throw new Error('expected a queued toolbar measurement frame');
      callback(performance.now());
    };

    const realRO = globalThis.ResizeObserver;
    const callbacks: ResizeObserverCallback[] = [];
    class Capturing {
      constructor(callback: ResizeObserverCallback) {
        callbacks.push(callback);
      }
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    }
    globalThis.ResizeObserver = Capturing as unknown as typeof ResizeObserver;
    try {
      let requiredWidth = 500;
      restoreDimensions = installToolbarDimensions(() => requiredWidth);
      const pane = createThreadPane();
      pane.replaceThread(makeThread());
      const result = render(ComposerToolbar, {
        props: toolbarProps(pane, { sendLabel: 'Go' }),
      });

      runNextFrame();
      await waitFor(() => {
        expect(result.getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'full');
      });

      requiredWidth = 640;
      await result.rerender(toolbarProps(pane, { sendLabel: 'Implement' }));
      for (const callback of callbacks) callback([], {} as ResizeObserver);

      expect(result.getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'full');
      runNextFrame();
      await waitFor(() => {
        expect(result.getByTestId('composer-toolbar')).toHaveAttribute('data-density', 'compact');
      });
    } finally {
      globalThis.ResizeObserver = realRO;
    }
  });
});
