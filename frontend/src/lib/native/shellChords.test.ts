import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { clearCommandRegistry } from '../stores/commandRegistry.svelte';
import { registerBrowserCommands } from '../stores/browserCommands.svelte';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../stores/browserCompanion.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../stores/companionPanes.svelte';
import { createPane, focusPane, resetPanesForTest } from '../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../stores/paneLayout.svelte';
import { makeCommandContext } from '../stores/builtinCommands.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';
import { focusMessage, installShellChords, RELOAD_APP_MESSAGE } from './shellChords.svelte';

const THREAD = 'thread-shell';

type ShellGlobals = { __aoShellChords?: number; __aoShellChord?: (name: string) => void };

describe('shell chords', () => {
  let posted: string[];
  let act: ReturnType<typeof vi.fn>;
  let uninstall: (() => void) | null;

  beforeEach(() => {
    posted = [];
    uninstall = null;
    vi.stubGlobal('chrome', { webview: { postMessage: (message: string) => posted.push(message) } });
    clearCommandRegistry();
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
    applyBrowserCompanionState({
      kind: 'state',
      threadId: THREAD,
      seq: 1,
      activePageId: 'p1',
      visible: true,
      pages: [{ id: 'p1', url: 'https://example.test/', title: 'P', canGoBack: false, canGoForward: false }],
    } as never);
    focusPane('main');
  });

  afterEach(() => {
    uninstall?.();
    delete (window as Window & ShellGlobals).__aoShellChords;
    vi.unstubAllGlobals();
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
  });

  function install(): void {
    uninstall = installShellChords(() => makeCommandContext(null, {}));
    flushSync();
  }

  function advertise(): void {
    (window as Window & ShellGlobals).__aoShellChords = 1;
    window.dispatchEvent(new Event('ao-shell-chords'));
  }

  it('posts nothing to a shell that does not route chords', () => {
    install();
    focusPane('browser-main');
    flushSync();
    expect(posted).toEqual([]);
  });

  it('reports the browser pane focus as it changes, and again when the shell advertises', () => {
    install();
    advertise();
    expect(posted).toEqual([focusMessage(false)]);
    focusPane('browser-main');
    flushSync();
    focusPane('main');
    flushSync();
    expect(posted).toEqual([focusMessage(false), focusMessage(true), focusMessage(false)]);
    // A new document's shell starts unfocused: the SPA repeats its state.
    advertise();
    expect(posted).toHaveLength(4);
    expect(posted.at(-1)).toBe(focusMessage(false));
  });

  it('reloads the focused page for a routed chord', () => {
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    (window as Window & ShellGlobals).__aoShellChord!('reload');
    expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'reload', pageId: 'p1' }));
    expect(posted).not.toContain(RELOAD_APP_MESSAGE);
  });

  it('hands a routed chord back when the browser pane is no longer focused', () => {
    install();
    advertise();
    (window as Window & ShellGlobals).__aoShellChord!('reload');
    expect(act).not.toHaveBeenCalled();
    expect(posted.at(-1)).toBe(RELOAD_APP_MESSAGE);
  });

  it('removes the handler and reports unfocused on uninstall', () => {
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    uninstall!();
    uninstall = null;
    expect((window as Window & ShellGlobals).__aoShellChord).toBeUndefined();
    expect(posted.at(-1)).toBe(focusMessage(false));
  });
});
