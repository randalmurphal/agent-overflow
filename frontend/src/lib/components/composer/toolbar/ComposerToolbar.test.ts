import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

import ComposerToolbar from './ComposerToolbar.svelte';
import { createThreadPane, type ThreadPane } from '../../../stores/thread.svelte';
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

function installToolbarDimensions(requiredFullWidth: number | (() => number), availableWidth = 520) {
  const clientSpy = vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get')
    .mockImplementation(function clientWidth(this: HTMLElement) {
      return this.dataset.testid === 'composer-toolbar' ? availableWidth : 0;
    });
  const scrollSpy = vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get')
    .mockImplementation(function scrollWidth(this: HTMLElement) {
      const width = typeof requiredFullWidth === 'function'
        ? requiredFullWidth()
        : requiredFullWidth;
      return this.dataset.testid === 'composer-toolbar' ? width : 0;
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
    restoreDimensions?.();
    restoreDimensions = undefined;
    restoreAnimationFrame?.();
    restoreAnimationFrame = undefined;
  });

  it('compacts collapsible toolbar labels when full contents overflow', async () => {
    restoreDimensions = installToolbarDimensions(640);
    const { getByTestId, getAllByText } = renderToolbar();

    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-compact', 'true');
    });
    expect(getAllByText('Plan').some((el) =>
      el.getAttribute('data-composer-toolbar-label') === 'collapsible',
    )).toBe(true);
    const collapsiblePlan = getAllByText('Plan').find((el) =>
      el.getAttribute('data-composer-toolbar-label') === 'collapsible',
    );
    expect(collapsiblePlan).toBeTruthy();
  });

  it('keeps full toolbar labels when full contents fit', async () => {
    restoreDimensions = installToolbarDimensions(500);
    const { getByTestId, getAllByText } = renderToolbar();

    await waitFor(() => {
      expect(getByTestId('composer-toolbar')).toHaveAttribute('data-compact', 'false');
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

  it('remeasures when toolbar text changes without a container resize', async () => {
    let requiredWidth = 500;
    restoreDimensions = installToolbarDimensions(() => requiredWidth);
    const pane = createThreadPane();
    pane.replaceThread(makeThread());
    const result = render(ComposerToolbar, {
      props: toolbarProps(pane, { sendLabel: 'Go' }),
    });

    await waitFor(() => {
      expect(result.getByTestId('composer-toolbar')).toHaveAttribute('data-compact', 'false');
    });

    requiredWidth = 640;
    await result.rerender(toolbarProps(pane, { sendLabel: 'Implement' }));

    await waitFor(() => {
      expect(result.getByTestId('composer-toolbar')).toHaveAttribute('data-compact', 'true');
    });
  });
});
