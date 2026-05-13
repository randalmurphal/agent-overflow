// Integration tests for the terminal drawer as part of the full App mount.
// These tests drive the toggle shortcut + the tab strip. Full xterm output
// rendering is exercised separately in the component-level tests — here
// we focus on the Wails binding calls and drawer lifecycle.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import type {
  TerminalHandle,
  TerminalSessionSummary,
} from '../../lib/types/terminal';
import { setBindingMock } from '../mocks/bindings-app';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

function summary(terminalID: string, overrides: Partial<TerminalSessionSummary> = {}): TerminalSessionSummary {
  return {
    terminalID,
    threadID: 'thread-1',
    shell: '/bin/bash',
    cwd: '/tmp/ws',
    rows: 24,
    cols: 80,
    pid: 1000,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
    ...overrides,
  };
}

function openedHandle(terminalID: string): TerminalHandle {
  return { terminalID, threadID: 'thread-1', summary: summary(terminalID) };
}

async function loadTerminalToggleKeybinding() {
  setBindingMock('GetKeybindings', async () => [
    { key: 'mod+j', command: 'terminal.toggle', when: 'hasActiveThread' },
  ]);
  const mod = await import('../../lib/stores/keybindings.svelte');
  await mod.loadKeybindings();
}

// Dispatch Mod+J directly on window. fireEvent.keyDown routes through the
// element's event API rather than window listeners, so use a raw KeyboardEvent
// dispatch to match the real app's global handler path.
function pressModJ() {
  window.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'j', ctrlKey: true, bubbles: true }),
  );
}

async function mountWithActiveThread(thread: Thread = makeThread({ title: 'Terminal Thread' })) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);
  // Terminal drawer lifecycle bindings. Defaults resolve with empty lists;
  // tests that care can override.
  setBindingMock('ListTerminals', async () => []);
  setBindingMock('OpenTerminal', async () => openedHandle('tmx-1'));
  setBindingMock('CloseTerminal', async () => {});
  setBindingMock('WriteTerminal', async () => {});
  setBindingMock('ResizeTerminal', async () => {});
  setBindingMock('GetTerminalReplay', async () => '');
  setBindingMock('RestartTerminal', async () => openedHandle('tmx-1'));

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  await loadTerminalToggleKeybinding();
  return rendered;
}

describe('App integration — terminal drawer', () => {
  beforeEach(() => {
    resetAppState();
    // xterm reads DOM APIs happy-dom doesn't always implement cleanly
    // (offsetHeight on <div>, clearRect on canvas ctx, etc). The unit
    // suite handles this with vi.mock; in integration tests we let xterm
    // instantiate and just swallow the noise.
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  it('Mod+J toggles the terminal drawer open', async () => {
    const rendered = await mountWithActiveThread();
    expect(rendered.queryByTestId('terminal-drawer')).toBeNull();
    pressModJ();
    await flush(10);
    await waitFor(() => {
      expect(rendered.getByTestId('terminal-drawer')).toBeInTheDocument();
    });
  });

  it('opens a new terminal when the + button is clicked', async () => {
    const rendered = await mountWithActiveThread();
    // Install AFTER mount so the helper's default doesn't clobber us.
    // Return a unique ID per call so the keyed each doesn't collide when the
    // auto-open + click both land.
    let nextId = 0;
    const openMock = setBindingMock('OpenTerminal', async (_threadID, _opts) => {
      nextId += 1;
      return openedHandle(`click-terminal-${nextId}`);
    });
    pressModJ();
    await flush(10);
    // Drawer mount auto-opens a first terminal when the list is empty, so
    // the default ListTerminals => [] path triggers OpenTerminal once.
    await waitFor(() => expect(openMock).toHaveBeenCalled());
    const firstCallCount = openMock.mock.calls.length;

    // Click the + button to open another.
    await fireEvent.click(rendered.getByTestId('terminal-open'));
    await flush(5);
    await waitFor(() =>
      expect(openMock.mock.calls.length).toBeGreaterThan(firstCallCount),
    );
  });

  it('closes a terminal via CloseTerminal and removes the tab', async () => {
    const rendered = await mountWithActiveThread();
    setBindingMock('ListTerminals', async () => [summary('tab-a')]);
    const closeMock = setBindingMock('CloseTerminal', async () => {});
    pressModJ();
    // Drawer's onMount calls ListTerminals async — let all microtasks
    // settle before asserting.
    await flush(30);

    await waitFor(
      () => {
        expect(rendered.getByTestId('terminal-tab-tab-a')).toBeInTheDocument();
      },
      { timeout: 3000 },
    );
    await fireEvent.click(rendered.getByTestId('terminal-tab-close-tab-a'));
    await flush(5);
    await waitFor(() => expect(closeMock).toHaveBeenCalledWith('tab-a'));
    await waitFor(() => {
      expect(rendered.queryByTestId('terminal-tab-tab-a')).toBeNull();
    });
  });

  it('selecting a different tab makes it active', async () => {
    const rendered = await mountWithActiveThread();
    setBindingMock('ListTerminals', async () => [summary('tab-a'), summary('tab-b')]);
    pressModJ();
    await flush(10);

    await waitFor(() => {
      expect(rendered.getByTestId('terminal-tab-tab-a')).toBeInTheDocument();
      expect(rendered.getByTestId('terminal-tab-tab-b')).toBeInTheDocument();
    });

    // Last tab is active by default. Click the first one.
    await fireEvent.click(rendered.getByTestId('terminal-tab-tab-a'));
    await flush();
    const aTab = rendered.getByTestId('terminal-tab-tab-a');
    expect(aTab.getAttribute('aria-selected')).toBe('true');
    const bTab = rendered.getByTestId('terminal-tab-tab-b');
    expect(bTab.getAttribute('aria-selected')).toBe('false');
  });

  it('collapse button hides the drawer', async () => {
    const rendered = await mountWithActiveThread();
    setBindingMock('ListTerminals', async () => [summary('tab-a')]);
    pressModJ();
    await flush(10);

    await waitFor(() => {
      expect(rendered.getByTestId('terminal-drawer')).toBeInTheDocument();
    });
    await fireEvent.click(rendered.getByTestId('terminal-collapse'));
    await flush();
    expect(rendered.queryByTestId('terminal-drawer')).toBeNull();
  });
});
