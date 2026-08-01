import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

interface FakePane {
  paneId: string;
  threadId: string | null;
  pendingApprovals: unknown[];
  pendingUserInputs: unknown[];
}

const state = vi.hoisted(() => ({
  focusedPaneId: null as string | null,
  panes: new Map<string, FakePaneShape>(),
  drafts: new Map<string, { hydrating: boolean; threadId: string | null }>(),
  terminalFocused: new Set<string>(),
  trapActive: false,
  revealed: [] as string[],
}));
// vi.hoisted runs before this module body, so the pane shape is structural.
type FakePaneShape = {
  paneId: string;
  threadId: string | null;
  pendingApprovals: unknown[];
  pendingUserInputs: unknown[];
};

vi.mock('../../stores/panes.svelte', () => ({
  getFocusedPaneId: () => state.focusedPaneId,
  getPane: (id: string) => state.panes.get(id),
  iterPanes: () => state.panes.values(),
  revealPane: (id: string) => { state.revealed.push(id); },
}));
vi.mock('../../stores/composerDraftRegistry.svelte', () => ({
  getComposerDraftForPane: (id: string) => state.drafts.get(id),
}));
vi.mock('../terminal/terminalStore.svelte', () => ({
  getTerminalFocused: (id: string) => state.terminalFocused.has(id),
}));
vi.mock('../../utils/focusTrap', () => ({
  hasActiveFocusTrap: () => state.trapActive,
}));

import {
  isClaimedFocus,
  isTypeToFocusKey,
  redirectTypingToFocusedComposer,
} from './typeToFocusComposer';

const OPEN_NOTHING = { workflowsOverlayOpen: false, anyModalOpen: false, anyPickerOpen: false };

function keyEvent(key: string, init: KeyboardEventInit & { keyCode?: number } = {}): KeyboardEvent {
  const { keyCode, isComposing, ...rest } = init;
  const ev = new KeyboardEvent('keydown', { key, ...rest });
  if (keyCode !== undefined) Object.defineProperty(ev, 'keyCode', { value: keyCode });
  if (isComposing) Object.defineProperty(ev, 'isComposing', { value: true });
  return ev;
}

function addPane(paneId: string, overrides: Partial<FakePane> = {}): FakePane {
  const pane: FakePane = {
    paneId,
    threadId: 't1',
    pendingApprovals: [],
    pendingUserInputs: [],
    ...overrides,
  };
  state.panes.set(paneId, pane);
  return pane;
}

function mountComposer(paneId: string, value = ''): HTMLTextAreaElement {
  const root = document.createElement('section');
  root.dataset.paneId = paneId;
  const textarea = document.createElement('textarea');
  textarea.setAttribute('aria-label', 'Message Input');
  textarea.value = value;
  root.appendChild(textarea);
  document.body.appendChild(root);
  return textarea;
}

/** Standard happy-path setup: focused thread pane, hydrated draft, composer mounted. */
function setupFocusedPane(paneId = 'main', draftValue = ''): HTMLTextAreaElement {
  addPane(paneId);
  state.focusedPaneId = paneId;
  state.drafts.set(paneId, { hydrating: false, threadId: 't1' });
  return mountComposer(paneId, draftValue);
}

beforeEach(() => {
  state.focusedPaneId = null;
  state.panes.clear();
  state.drafts.clear();
  state.terminalFocused.clear();
  state.trapActive = false;
  state.revealed = [];
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

describe('isTypeToFocusKey', () => {
  it('accepts bare printable characters, shift included', () => {
    expect(isTypeToFocusKey(keyEvent('a'))).toBe(true);
    expect(isTypeToFocusKey(keyEvent('A', { shiftKey: true }))).toBe(true);
    expect(isTypeToFocusKey(keyEvent('?', { shiftKey: true }))).toBe(true);
    expect(isTypeToFocusKey(keyEvent('1'))).toBe(true);
  });

  it('rejects chords, space, named keys, and IME preedit', () => {
    expect(isTypeToFocusKey(keyEvent('a', { metaKey: true }))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('a', { ctrlKey: true }))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('å', { altKey: true }))).toBe(false);
    expect(isTypeToFocusKey(keyEvent(' '))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('Enter'))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('ArrowDown'))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('F2'))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('Dead'))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('a', { isComposing: true }))).toBe(false);
    expect(isTypeToFocusKey(keyEvent('Process', { keyCode: 229 }))).toBe(false);
  });
});

describe('isClaimedFocus', () => {
  function mountFocusable(html: string, focusSelector: string): Element {
    document.body.innerHTML = html;
    return document.querySelector(focusSelector)!;
  }

  it('does not claim body or plain scroll containers', () => {
    expect(isClaimedFocus(null)).toBe(false);
    expect(isClaimedFocus(document.body)).toBe(false);
    // The timeline scroll surface is tabindex="-1" and takes focus on click;
    // typing from there is exactly the feature's home case.
    const scroller = mountFocusable('<div tabindex="-1" id="scroller"></div>', '#scroller');
    expect(isClaimedFocus(scroller)).toBe(false);
  });

  it('claims activatable controls and their descendants', () => {
    expect(isClaimedFocus(mountFocusable('<button id="b">x</button>', '#b'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<button><span id="s">x</span></button>', '#s'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<div role="button" tabindex="0" id="r">x</div>', '#r'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<a href="#x" id="a">x</a>', '#a'))).toBe(true);
  });

  it('claims anything inside dialogs, popovers, and toolbars', () => {
    expect(isClaimedFocus(mountFocusable('<div role="dialog"><div id="d">x</div></div>', '#d'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<div aria-modal="true"><div id="m">x</div></div>', '#m'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<div data-popover><div id="p">x</div></div>', '#p'))).toBe(true);
    expect(isClaimedFocus(mountFocusable('<div role="toolbar"><div id="t" tabindex="0">x</div></div>', '#t'))).toBe(true);
  });
});

describe('redirectTypingToFocusedComposer', () => {
  it('focuses the composer with the caret at the end of the resumed draft', () => {
    const textarea = setupFocusedPane('main', 'resumed draft');
    const focus = vi.spyOn(textarea, 'focus');

    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(true);

    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    expect(textarea.selectionStart).toBe('resumed draft'.length);
    expect(textarea.selectionEnd).toBe('resumed draft'.length);
    expect(state.revealed).toEqual(['main']);
  });

  it('never calls preventDefault — the browser must insert the character natively', () => {
    setupFocusedPane();
    const ev = keyEvent('a');
    const prevent = vi.spyOn(ev, 'preventDefault');
    expect(redirectTypingToFocusedComposer(ev, OPEN_NOTHING)).toBe(true);
    expect(prevent).not.toHaveBeenCalled();
  });

  it('stands down while an overlay, modal, or picker is open', () => {
    setupFocusedPane();
    expect(redirectTypingToFocusedComposer(keyEvent('a'), { ...OPEN_NOTHING, workflowsOverlayOpen: true })).toBe(false);
    expect(redirectTypingToFocusedComposer(keyEvent('a'), { ...OPEN_NOTHING, anyModalOpen: true })).toBe(false);
    expect(redirectTypingToFocusedComposer(keyEvent('a'), { ...OPEN_NOTHING, anyPickerOpen: true })).toBe(false);
  });

  it('stands down while a focus trap is active', () => {
    setupFocusedPane();
    state.trapActive = true;
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('stands down when an interactive element has focus', () => {
    setupFocusedPane();
    const button = document.createElement('button');
    document.body.appendChild(button);
    button.focus();
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('yields to a pending user-input prompt in ANY pane (bare digits answer it)', () => {
    setupFocusedPane('main');
    addPane('pane-2', { pendingUserInputs: [{}] });
    expect(redirectTypingToFocusedComposer(keyEvent('1'), OPEN_NOTHING)).toBe(false);
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('yields to a pending approval in the focused pane', () => {
    const textarea = setupFocusedPane();
    state.panes.get('main')!.pendingApprovals = [{}];
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
    expect(document.activeElement).not.toBe(textarea);
  });

  it('ignores a focused companion pane (raw id, not resolved-to-source)', () => {
    setupFocusedPane('main');
    // Companion panes live in the layout but not the ThreadPane registry.
    state.focusedPaneId = 'companion-review';
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('does not steal from a focused terminal in the pane', () => {
    setupFocusedPane('main');
    state.terminalFocused.add('main');
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('waits out draft hydration and thread mismatches', () => {
    setupFocusedPane('main');
    state.drafts.set('main', { hydrating: true, threadId: 't1' });
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);

    state.drafts.set('main', { hydrating: false, threadId: 't-stale' });
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('no-ops when the composer is missing or disabled', () => {
    addPane('main');
    state.focusedPaneId = 'main';
    state.drafts.set('main', { hydrating: false, threadId: 't1' });
    // No textarea mounted (discussion-mode pane).
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);

    const textarea = mountComposer('main');
    textarea.disabled = true;
    expect(redirectTypingToFocusedComposer(keyEvent('a'), OPEN_NOTHING)).toBe(false);
  });

  it('ignores non-qualifying keys outright', () => {
    const textarea = setupFocusedPane();
    expect(redirectTypingToFocusedComposer(keyEvent('Enter'), OPEN_NOTHING)).toBe(false);
    expect(redirectTypingToFocusedComposer(keyEvent(' '), OPEN_NOTHING)).toBe(false);
    expect(redirectTypingToFocusedComposer(keyEvent('a', { metaKey: true }), OPEN_NOTHING)).toBe(false);
    expect(document.activeElement).not.toBe(textarea);
  });
});
