import { describe, it, expect, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import TerminalTabStrip from './TerminalTabStrip.svelte';
import {
  createThreadTerminalState,
  type ThreadTerminalStateHandle,
} from './terminalStore.svelte';
import type { TerminalSessionSummary } from '../../types/terminal';

// TerminalTabStrip is purely presentational: props in, callbacks out. It never
// touches the Wails bindings (TerminalSurface owns the RefreshTerminal call), so
// we render it in isolation with a real (but headless) terminal-state handle and
// spy callbacks — no xterm, no binding mocks. Omitting `workspacePath` keeps the
// EditorLink (and its bindings) out of the tree.

function makeSummary(terminalID: string): TerminalSessionSummary {
  return {
    terminalID,
    threadID: 'thread-A',
    shell: '/bin/bash',
    cwd: '/tmp',
    rows: 24,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

function handleWithTabs(...ids: string[]): ThreadTerminalStateHandle {
  const handle = createThreadTerminalState();
  for (const id of ids) handle.addTab(makeSummary(id));
  return handle;
}

const noop = () => {};

describe('TerminalTabStrip refresh and find affordances', () => {
  it('renders the refresh button and invokes onRefresh on click when a terminal is open', async () => {
    const onRefresh = vi.fn();
    const { getByTestId } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
      onRefresh,
    });

    await fireEvent.click(getByTestId('terminal-refresh'));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['hides the refresh button when there are no tabs to refresh', 'terminal-refresh', [], { onRefresh: noop }],
    ['hides the refresh button when the host does not wire onRefresh', 'terminal-refresh', ['t1'], {}],
    ['hides the find button with no tabs', 'terminal-find', [], { onFind: noop }],
    ['hides the find button when the host does not wire onFind', 'terminal-find', ['t1'], {}],
  ])('%s', (_name, testId, tabs, wired) => {
    const { queryByTestId } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs(...tabs),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
      ...wired,
    });

    expect(queryByTestId(testId)).toBeNull();
  });

  it('renders a labeled find button with a tooltip that invokes onFind', async () => {
    const onFind = vi.fn();
    const { getByTestId } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
      onFind,
    });

    const button = getByTestId('terminal-find');
    expect(button.getAttribute('aria-label')).toBe('Find in Terminal');
    expect(button.getAttribute('title')).toBe('Find in Terminal');
    // The shared primitive, so it carries the compact hit target.
    expect(button.hasAttribute('data-icon-button')).toBe(true);
    await fireEvent.click(button);
    expect(onFind).toHaveBeenCalledTimes(1);
  });
});

// Middle-click close + right-click menu. Both are strip-local: they resolve
// the target tabs from `handle.tabs` and hand each id to the same `onClose`
// the ✕ button uses, so the host keeps owning CloseTerminal, the focus latch,
// and the collapse-on-empty rule.
describe('TerminalTabStrip close affordances', () => {
  function middleClick(el: HTMLElement): void {
    el.dispatchEvent(new MouseEvent('auxclick', { button: 1, bubbles: true, cancelable: true }));
  }

  async function openMenuOn(el: HTMLElement): Promise<void> {
    await fireEvent.contextMenu(el, { clientX: 10, clientY: 10 });
  }

  function menuItem(container: HTMLElement | Document, label: string): HTMLElement {
    const found = Array.from(container.querySelectorAll<HTMLElement>('[role="menuitem"]'))
      .find((el) => el.textContent?.trim() === label);
    if (!found) throw new Error(`no menu item labelled ${label}`);
    return found;
  }

  it('closes a tab on middle click without selecting it', () => {
    const onClose = vi.fn();
    const onSelect = vi.fn();
    const { getByTestId } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1', 't2'),
      onOpen: noop,
      onClose,
      onSelect,
    });

    middleClick(getByTestId('terminal-tab-t2'));

    expect(onClose).toHaveBeenCalledExactlyOnceWith('t2');
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('ignores auxclick from other non-primary buttons', () => {
    const onClose = vi.fn();
    const { getByTestId } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose,
      onSelect: noop,
    });

    getByTestId('terminal-tab-t1').dispatchEvent(
      new MouseEvent('auxclick', { button: 2, bubbles: true, cancelable: true }),
    );

    expect(onClose).not.toHaveBeenCalled();
  });

  it('suppresses the native menu and closes only the clicked tab from Close', async () => {
    const onClose = vi.fn();
    const { getByTestId, baseElement } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1', 't2', 't3'),
      onOpen: noop,
      onClose,
      onSelect: noop,
    });

    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    getByTestId('terminal-tab-t2').dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    await Promise.resolve();

    await fireEvent.click(menuItem(baseElement as HTMLElement, 'Close'));
    expect(onClose).toHaveBeenCalledExactlyOnceWith('t2');
    expect((baseElement as HTMLElement).querySelector('[role="menu"]')).toBeNull();
  });

  it.each([
    ['closes every other tab from Close Others', 'Close Others', 't2', ['t1', 't3']],
    ['closes only the tabs after the target from Close to the Right', 'Close to the Right', 't2', ['t3']],
    ['closes all tabs from Close All, whichever tab was right-clicked', 'Close All', 't3', ['t1', 't2', 't3']],
  ])('%s', async (_name, label, target, closed) => {
    const onClose = vi.fn();
    const { getByTestId, baseElement } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1', 't2', 't3'),
      onOpen: noop,
      onClose,
      onSelect: noop,
    });

    await openMenuOn(getByTestId(`terminal-tab-${target}`));
    await fireEvent.click(menuItem(baseElement as HTMLElement, label));

    expect(onClose.mock.calls.map(([id]) => id)).toEqual(closed);
  });

  it('disables the rows that would close nothing', async () => {
    const { getByTestId, baseElement } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
    });

    await openMenuOn(getByTestId('terminal-tab-t1'));
    const root = baseElement as HTMLElement;
    expect(menuItem(root, 'Close Others').getAttribute('aria-disabled')).toBe('true');
    expect(menuItem(root, 'Close to the Right').getAttribute('aria-disabled')).toBe('true');
    expect(menuItem(root, 'Close').getAttribute('aria-disabled')).toBeNull();
    expect(menuItem(root, 'Close All').getAttribute('aria-disabled')).toBeNull();
  });

  it('drops the menu when its tab disappears from under it', async () => {
    const handle = handleWithTabs('t1', 't2');
    const { getByTestId, baseElement } = render(TerminalTabStrip, { backend: '',
      handle,
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
    });

    await openMenuOn(getByTestId('terminal-tab-t2'));
    expect((baseElement as HTMLElement).querySelector('[role="menu"]')).not.toBeNull();

    handle.removeTab('t2');
    await tick();

    expect((baseElement as HTMLElement).querySelector('[role="menu"]')).toBeNull();
  });

  it('dismisses the menu on Escape', async () => {
    const { getByTestId, baseElement } = render(TerminalTabStrip, { backend: '',
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
    });

    await openMenuOn(getByTestId('terminal-tab-t1'));
    expect((baseElement as HTMLElement).querySelector('[role="menu"]')).not.toBeNull();

    await fireEvent.keyDown(document, { key: 'Escape' });
    expect((baseElement as HTMLElement).querySelector('[role="menu"]')).toBeNull();
  });
});
