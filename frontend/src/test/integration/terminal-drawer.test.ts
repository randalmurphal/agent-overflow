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
  getTerminalFocused,
  notifyTerminalFocus,
} from '../../lib/components/terminal/terminalStore.svelte';
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
  setBindingMock('GetKeybindings', async () => ({
    bindings: [{ key: 'mod+j', command: 'terminal.toggle', when: 'hasActiveThread' }],
  }));
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

// Dispatch Mod+J ON a specific element (bubbles up to the same window handler)
// so `ev.target` is that element. Used to drive handleGlobalKeydown's
// editable-target branch — the path a chord takes when the focused element is a
// <textarea>, as the live xterm helper textarea is.
function pressModJOn(target: EventTarget) {
  target.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'j', ctrlKey: true, bubbles: true }),
  );
}

// The drawer is a lazy import; its first-ever load in a suite run pays the
// on-demand transform, which can exceed waitFor's default 1s under full suite
// load. Every "the drawer opened" assertion goes through here so the budget
// lives in one place.
async function waitForTerminalDrawer(rendered: { getByTestId: (id: string) => HTMLElement }) {
  await waitFor(() => {
    expect(rendered.getByTestId('terminal-drawer')).toBeInTheDocument();
  }, { timeout: 5000 });
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
    await waitForTerminalDrawer(rendered);
  });

  it('Mod+J closes the drawer when focus sits in a terminal-style textarea', async () => {
    // The real close trigger is the focused xterm <textarea>:
    // handleGlobalKeydown treats editable targets specially and only fires
    // terminal.toggle from inside one because the command is in
    // EDITABLE_REACHABLE_COMMANDS. A synthetic textarea is a faithful proxy —
    // that gate keys off tagName === 'TEXTAREA'. This pins the load-bearing
    // `editableReachable: true` flag on terminal.toggle, which the unit tests
    // (calling runCommand directly) bypass: drop the flag and this fails while
    // every unit test stays green.
    const rendered = await mountWithActiveThread();
    pressModJ();
    await flush(10);
    await waitForTerminalDrawer(rendered);

    const textarea = document.createElement('textarea');
    document.body.appendChild(textarea);
    textarea.focus();
    try {
      pressModJOn(textarea);
      await flush(10);
      await waitFor(() => {
        expect(rendered.queryByTestId('terminal-drawer')).toBeNull();
      });
    } finally {
      document.body.removeChild(textarea);
    }
  });

  it('Mod+W does not close the pane from inside a focused terminal (werase passes through)', async () => {
    // The user-reported bug: ctrl/cmd+w (the close-pane chord) acting inside the
    // terminal instead of reaching the shell as werase. pane.close is
    // editableReachable, so its ONLY guard against firing from the focused xterm
    // <textarea> is `when: '!terminalFocus'`. handleGlobalKeydown dispatches
    // against a MEMOIZED $derived command context, and the terminal-focus
    // registry is a plain counter (not $state), so this pins that terminalFocus
    // is observed FRESH at keypress time — a stale-false context would let
    // pane.close through and swallow werase. Routes through the real editable-gate
    // path; a runCommand unit test rebuilds the context each call and cannot
    // reproduce the staleness.
    const rendered = await mountWithActiveThread();
    setBindingMock('GetKeybindings', async () => ({
      bindings: [
        { key: 'mod+j', command: 'terminal.toggle', when: 'hasActiveThread' },
        { key: 'mod+w', command: 'pane.close', when: '!terminalFocus' },
      ],
    }));
    const kb = await import('../../lib/stores/keybindings.svelte');
    await kb.loadKeybindings();

    pressModJ();
    await flush(10);
    await waitForTerminalDrawer(rendered);

    // Mirror the live xterm grabbing DOM focus: TerminalBody bumps the focus
    // registry on focusin, and that focusin bubbles to ChatView's
    // onfocusin={() => focusPane(paneId)} — a no-op when the pane is already
    // focused. That no-op is the exact condition that strands the memoized
    // context, so we DON'T move focusedPaneId here. The active thread opened in
    // the main pane, so register focus under 'main' (the registry is pane-keyed).
    notifyTerminalFocus('main', true);

    const textarea = document.createElement('textarea');
    document.body.appendChild(textarea);
    textarea.focus();
    try {
      const ev = new KeyboardEvent('keydown', {
        key: 'w',
        ctrlKey: true,
        bubbles: true,
        cancelable: true,
      });
      textarea.dispatchEvent(ev);
      await flush(10);
      // Unconsumed → propagates to the xterm as werase. defaultPrevented would
      // mean handleGlobalKeydown ran pane.close.
      expect(ev.defaultPrevented).toBe(false);
      // And the drawer is untouched — the pane did not close.
      expect(rendered.getByTestId('terminal-drawer')).toBeInTheDocument();
    } finally {
      document.body.removeChild(textarea);
      notifyTerminalFocus('main', false);
    }
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

    await waitForTerminalDrawer(rendered);
    await fireEvent.click(rendered.getByTestId('terminal-collapse'));
    await flush();
    expect(rendered.queryByTestId('terminal-drawer')).toBeNull();
  });

  // Cold-open focus regression. Opening the terminal must land DOM focus in
  // the xterm even on the FIRST open, when ThreadTerminalDrawer arrives via a
  // lazy dynamic import. runTerminalToggle latches pane.requestTerminalFocus()
  // before showing the drawer; the drawer reads-and-clears that intent in
  // onMount — however many frames the import takes — and focuses TerminalBody
  // once it binds, which bumps the focus registry via xterm's focusin.
  //
  // The old one-shot FOCUS_TERMINAL_EVENT fired one rAF after open, before the
  // lazy drawer had mounted and registered its window listener, so the event
  // was lost and focus never moved. This is the exact race that test asserts
  // against — and that happy-dom couldn't even reach before, since it mounts
  // the drawer after the rAF. The pane-owned flag removes the race entirely.
  it('lands focus in the terminal on a cold first open', async () => {
    const rendered = await mountWithActiveThread();
    // The active thread opened in the main pane; the real TerminalBody registers
    // its focus under that pane id, so query the registry for 'main'.
    expect(getTerminalFocused('main')).toBe(false);

    pressModJ();

    await waitForTerminalDrawer(rendered);
    // The chain crosses the lazy import, async OpenTerminal/replay, and two
    // rAFs before xterm's focusin bumps the registry. waitFor polls on real
    // timers so those macrotasks resolve.
    await waitFor(() => {
      expect(getTerminalFocused('main')).toBe(true);
    });
  });
});

// NOTE: end-to-end "DOM focus lands in the xterm on open" IS now asserted
// above ("lands focus in the terminal on a cold first open"). It used to be
// unassertable: the old handoff was a one-shot FOCUS_TERMINAL_EVENT dispatched
// one rAF after setShowTerminal(true), and happy-dom mounts the (lazily
// imported) drawer AFTER that rAF, so the event was missed and the chain never
// started — the same mount-vs-event race that broke cold opens in the real
// browser. The pane-owned consume-once focus flag (pane.requestTerminalFocus
// / consumeTerminalFocusRequest) removed the race: the intent waits for the
// drawer however late it mounts, so the chain runs deterministically here.
// The separate REGRESSION of the composer stealing focus back from a freshly
// focused terminal on a placeholder thread's first open is covered as a unit
// test in Composer.test.ts ("does not steal focus from a terminal that already
// owns it on entry").
