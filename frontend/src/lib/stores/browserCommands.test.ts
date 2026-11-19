import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { clearCommandRegistry, getCommand, runCommand, type CommandContext } from './commandRegistry.svelte';
import { isBrowserPaneFocused, registerBrowserCommands } from './browserCommands.svelte';
import {
  applyBrowserCompanionState,
  resetBrowserCompanionForTest,
  setBrowserPaneMountForTest,
} from './browserCompanion.svelte';
import { addressFocusIntent, registerBrowserAddressBar } from './browserAddressFocus.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from './companionPanes.svelte';
import { createPane, focusPane, getFocusedPaneId, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { dispatchKey, resetKeybindingsStore, setKeybindingsForTest } from './keybindings.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
import { evaluateWhen, parseWhen } from './keybindingParser';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { closeFocusedBrowserTab } from './browserActions';
import { getCompactScreen, noteCompactPaneShown, setCompactLayoutForTest, showCompactThread } from './layoutMode.svelte';
import { makeThread } from '../../test/helpers/chat';

const THREAD = 'thread-browser';
const COMMANDS = ['browser.newTab', 'browser.focusAddress', 'browser.reload', 'browser.nextTab', 'browser.prevTab', 'browser.reopenTab'];

function page(id: string) {
  return { id, url: `https://example.test/${id}`, title: `Page ${id}`, canGoBack: false, canGoForward: false };
}

let seq = 0;
function pushState(activePageId: string, ids = ['a', 'b', 'c']): void {
  seq += 1;
  applyBrowserCompanionState({ kind: 'state', threadId: THREAD, seq, activePageId, visible: true, pages: ids.map(page) } as never);
}

function ctx(): CommandContext {
  return makeCommandContext(null, {});
}

function key(init: KeyboardEventInit): KeyboardEvent {
  return new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init });
}

describe('browser commands', () => {
  let act: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    seq = 0;
    clearCommandRegistry();
    resetKeybindingsStore();
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 }]);
    createPane('main').replaceThread(makeThread({ id: THREAD }));
    openCompanion('main', 'browser');
    act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, seq: 0, pages: [] })));
    registerBrowserCommands();
    setKeybindingsForTest([...KEYBINDING_DEFAULTS]);
    pushState('b');
  });

  afterEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    resetKeybindingsStore();
  });

  it('are live only while a browser pane is the focused pane and nothing is above it', () => {
    focusPane('main');
    expect(isBrowserPaneFocused()).toBe(false);
    expect(ctx().flags.browserPaneFocused).toBe(false);
    focusPane('browser-main');
    expect(isBrowserPaneFocused()).toBe(true);
    expect(ctx().flags.browserPaneFocused).toBe(true);
    for (const id of COMMANDS) {
      const command = getCommand(id);
      expect(command?.editableReachable, id).toBe(true);
      const when = parseWhen(command!.when!);
      expect(evaluateWhen(when, { browserPaneFocused: true, overlayOpen: false } as never), id).toBe(true);
      expect(evaluateWhen(when, { browserPaneFocused: false, overlayOpen: false } as never), id).toBe(false);
      expect(evaluateWhen(when, { browserPaneFocused: true, overlayOpen: true } as never), id).toBe(false);
    }
  });

  it('ship the desktop-browser chords, scoped to the browser pane', () => {
    const rows = KEYBINDING_DEFAULTS.filter((rule) => rule.command.startsWith('browser.'));
    expect(rows.map((rule) => [rule.command, rule.key, rule.when])).toEqual([
      ['browser.newTab', 'mod+t', 'browserPaneFocused'],
      ['browser.focusAddress', 'mod+l', 'browserPaneFocused'],
      ['browser.reload', 'mod+r', 'browserPaneFocused'],
      ['browser.reload', 'f5', 'browserPaneFocused'],
      ['browser.nextTab', 'ctrl+tab', 'browserPaneFocused'],
      ['browser.prevTab', 'ctrl+shift+tab', 'browserPaneFocused'],
      ['browser.reopenTab', 'mod+shift+t', 'browserPaneFocused'],
    ]);
  });

  it('next and previous tab each send one relative step, also back to back with no state between', () => {
    focusPane('browser-main');
    runCommand('browser.nextTab', ctx());
    runCommand('browser.nextTab', ctx());
    runCommand('browser.prevTab', ctx());
    expect(act.mock.calls.map(([thread, action]) => [thread, action.kind, action.index, action.pageId])).toEqual([
      [THREAD, 'step', 1, ''],
      [THREAD, 'step', 1, ''],
      [THREAD, 'step', -1, ''],
    ]);
  });

  it('reload reloads the active tab', () => {
    focusPane('browser-main');
    runCommand('browser.reload', ctx());
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'reload', pageId: 'b' }));
  });

  it('reopen asks the backend for the last closed tab, from the chord too', () => {
    focusPane('browser-main');
    runCommand('browser.reopenTab', ctx());
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'reopen' }));
    act.mockClear();
    expect(dispatchKey(key({ key: 'T', ctrlKey: true, shiftKey: true }), ctx(), { isMac: false })).toBe(true);
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'reopen' }));
    focusPane('main');
    act.mockClear();
    expect(dispatchKey(key({ key: 'T', ctrlKey: true, shiftKey: true }), ctx(), { isMac: false })).toBe(false);
    expect(act).not.toHaveBeenCalled();
  });

  it('new tab opens a tab the user asked for, which takes the address bar', () => {
    focusPane('browser-main');
    runCommand('browser.newTab', ctx());
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'new' }));
    expect(addressFocusIntent(THREAD)).toBe('');
  });

  it('focus address focuses the mounted pane\'s bar, and nothing once it is gone', () => {
    focusPane('browser-main');
    const focus = vi.fn();
    const release = registerBrowserAddressBar(THREAD, focus);
    runCommand('browser.focusAddress', ctx());
    expect(focus).toHaveBeenCalledTimes(1);
    release();
    runCommand('browser.focusAddress', ctx());
    expect(focus).toHaveBeenCalledTimes(1);
  });

  it('dispatch the chords only for the focused browser pane', () => {
    focusPane('main');
    expect(dispatchKey(key({ key: 'Tab', ctrlKey: true }), ctx(), { isMac: false })).toBe(false);
    expect(dispatchKey(key({ key: 't', ctrlKey: true }), ctx(), { isMac: false })).toBe(false);
    expect(dispatchKey(key({ key: 'F5' }), ctx(), { isMac: false })).toBe(false);
    expect(act).not.toHaveBeenCalled();

    focusPane('browser-main');
    expect(dispatchKey(key({ key: 'Tab', ctrlKey: true }), ctx(), { isMac: false })).toBe(true);
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'step', index: 1 }));
    expect(dispatchKey(key({ key: 'Tab', ctrlKey: true, shiftKey: true }), ctx(), { isMac: false })).toBe(true);
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'step', index: -1 }));
    expect(dispatchKey(key({ key: 'F5' }), ctx(), { isMac: false })).toBe(true);
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'reload', pageId: 'b' }));
    expect(dispatchKey(key({ key: 'r', metaKey: true }), ctx(), { isMac: true })).toBe(true);
    expect(dispatchKey(key({ key: 't', metaKey: true }), ctx(), { isMac: true })).toBe(true);
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'new' }));
  });

  // A browser focused on the desktop keeps logical focus through a switch
  // to compact and back to the list, where geometry hides its page.
  it('take no command for a focused browser pane compact hides', () => {
    focusPane('browser-main');
    setCompactLayoutForTest(true);
    try {
      expect(getCompactScreen()).toBe('list');
      expect(isBrowserPaneFocused()).toBe(false);
      expect(dispatchKey(key({ key: 'F5' }), ctx(), { isMac: false })).toBe(false);
      expect(dispatchKey(key({ key: 'Tab', ctrlKey: true }), ctx(), { isMac: false })).toBe(false);
      expect(closeFocusedBrowserTab()).toBe(false);
      expect(act).not.toHaveBeenCalled();

      showCompactThread();
      noteCompactPaneShown('main');
      expect(isBrowserPaneFocused()).toBe(false);
      expect(closeFocusedBrowserTab()).toBe(false);

      noteCompactPaneShown('browser-main');
      expect(isBrowserPaneFocused()).toBe(true);
      expect(dispatchKey(key({ key: 'F5' }), ctx(), { isMac: false })).toBe(true);
      expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'reload', pageId: 'b' }));
    } finally {
      setCompactLayoutForTest(false);
    }
    expect(isBrowserPaneFocused()).toBe(true);
  });

  it('a chord pressed in the page focuses the browser pane and runs there', () => {
    setBrowserPaneMountForTest(THREAD, 'mount-1');
    focusPane('main');
    const dispatcher = (event: KeyboardEvent): void => {
      dispatchKey(event, ctx(), { isMac: false });
    };
    window.addEventListener('keydown', dispatcher);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: THREAD, mount: 'mount-1', accelerator: { key: 'tab', ctrl: true } } as never);
    } finally {
      window.removeEventListener('keydown', dispatcher);
    }
    expect(getFocusedPaneId()).toBe('browser-main');
    expect(act).toHaveBeenLastCalledWith(THREAD, expect.objectContaining({ kind: 'step', index: 1 }));
  });
});
