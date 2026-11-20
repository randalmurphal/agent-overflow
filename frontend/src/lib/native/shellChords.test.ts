import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { clearCommandRegistry, type CommandContext } from '../stores/commandRegistry.svelte';
import { registerBrowserCommands } from '../stores/browserCommands.svelte';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../stores/browserCompanion.svelte';
import { attachBrowserPaneForTest } from '../../test/helpers/browserPane';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../stores/companionPanes.svelte';
import { createPane, focusPane, resetPanesForTest } from '../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../stores/paneLayout.svelte';
import { makeCommandContext } from '../stores/builtinCommands.svelte';
import { dispatchKey, resetKeybindingsStore, setKeybindingsForTest, type KeybindingRule } from '../stores/keybindings.svelte';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';
import { installShellChords, reloadAppMessage } from './shellChords.svelte';

const THREAD = 'thread-shell';

type ShellGlobals = { __aoShellChords?: number; __aoShellChord?: (name: string) => void };

/** The defaults with the mod+r browser reload row replaced. */
function withReloadChord(replacement: KeybindingRule | null): KeybindingRule[] {
  const rules = KEYBINDING_DEFAULTS.filter((rule) => !(rule.key === 'mod+r' && rule.command === 'browser.reload'));
  return replacement ? [...rules, replacement] : rules;
}

describe('shell chords', () => {
  let posted: string[];
  let act: ReturnType<typeof vi.fn>;
  let uninstall: (() => void) | null;
  const ctx = (): CommandContext => makeCommandContext(null, {});
  // The App's keydown dispatcher.
  const appKeydown = (event: KeyboardEvent): void => {
    if (dispatchKey(event, ctx())) event.preventDefault();
  };

  beforeEach(() => {
    posted = [];
    uninstall = null;
    vi.stubGlobal('chrome', { webview: { postMessage: (message: string) => posted.push(message) } });
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
    applyBrowserCompanionState({
      kind: 'state',
      threadId: THREAD,
      seq: 1,
      activePageId: 'p1',
      visible: true,
      pages: [{ id: 'p1', url: 'https://example.test/', title: 'P', canGoBack: false, canGoForward: false }],
    } as never);
    focusPane('main');
    window.addEventListener('keydown', appKeydown);
  });

  afterEach(() => {
    window.removeEventListener('keydown', appKeydown);
    uninstall?.();
    delete (window as Window & ShellGlobals).__aoShellChords;
    vi.unstubAllGlobals();
    resetKeybindingsStore();
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
  });

  function install(): void {
    uninstall = installShellChords(ctx);
    flushSync();
  }

  function advertise(document = 1): void {
    (window as Window & ShellGlobals).__aoShellChords = document;
    window.dispatchEvent(new Event('ao-shell-chords'));
  }

  /** The route reports posted, as document:answer, each numbered after the last. */
  function routes(): string[] {
    let last = 0;
    const out: string[] = [];
    for (const message of posted) {
      const match = /^ao:shell:route-reload:(\d+):(\d+):([01])$/.exec(message);
      if (!match) continue;
      const seq = Number(match[2]);
      expect(seq).toBeGreaterThan(last);
      last = seq;
      out.push(`${match[1]}:${match[3] === '1'}`);
    }
    return out;
  }

  function routedChord(): void {
    (window as Window & ShellGlobals).__aoShellChord!('reload');
  }

  it('posts nothing to a shell that does not route chords', () => {
    install();
    focusPane('browser-main');
    flushSync();
    expect(posted).toEqual([]);
  });

  it('asks for the chord while the browser pane is focused, and again when the shell advertises', () => {
    install();
    advertise();
    expect(routes()).toEqual(['1:false']);
    focusPane('browser-main');
    flushSync();
    focusPane('main');
    flushSync();
    expect(routes()).toEqual(['1:false', '1:true', '1:false']);
    // A new document's shell starts not routing: the SPA repeats its answer
    // under that document's number.
    advertise(2);
    expect(posted).toHaveLength(4);
    expect(routes().at(-1)).toBe('2:false');
  });

  it('replays the routed chord through the keybinding dispatcher: the default reloads the page', async () => {
    await attachBrowserPaneForTest(THREAD, 'mount-1');
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    routedChord();
    expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'reload' }));
    expect(posted).not.toContain(reloadAppMessage(1));
  });

  it('leaves the chord to the shell when the effective bindings unbind it', () => {
    setKeybindingsForTest(withReloadChord({ key: 'mod+e', command: 'browser.reload', when: 'browserPaneFocused' }));
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    expect(routes()).toEqual(['1:false']);
    // A chord the shell still hands over (a report in flight) goes back.
    routedChord();
    expect(act).not.toHaveBeenCalled();
    expect(posted.at(-1)).toBe(reloadAppMessage(1));
  });

  it('runs the command the chord is rebound to there', async () => {
    await attachBrowserPaneForTest(THREAD, 'mount-1');
    setKeybindingsForTest(withReloadChord({ key: 'mod+r', command: 'browser.newTab', when: 'browserPaneFocused' }));
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    expect(routes().at(-1)).toBe('1:true');
    routedChord();
    expect(act).toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'new' }));
    expect(act).not.toHaveBeenCalledWith(THREAD, expect.objectContaining({ kind: 'reload' }));
  });

  it('hands a routed chord back when nothing runs it', () => {
    install();
    advertise();
    routedChord();
    expect(act).not.toHaveBeenCalled();
    expect(posted.at(-1)).toBe(reloadAppMessage(1));
  });

  it('removes the handler and stops routing on uninstall', () => {
    install();
    advertise();
    focusPane('browser-main');
    flushSync();
    uninstall!();
    uninstall = null;
    expect((window as Window & ShellGlobals).__aoShellChord).toBeUndefined();
    expect(routes().at(-1)).toBe('1:false');
  });
});
