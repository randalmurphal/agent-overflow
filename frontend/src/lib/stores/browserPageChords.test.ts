// Chords pressed in a browser page, as the backend routes them back to the
// pane mount that showed the page: replayed through the window's keybinding
// dispatcher in that pane, or dropped when it no longer has the keyboard.

import { describe, beforeEach, expect, it, vi } from 'vitest';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from './browserCompanion.svelte';
import { demotePane, promotePane } from './paneLayout.svelte';
import { focusPane, getFocusedPaneId, getPane } from './panes.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import { notifyTerminalFocus } from '../components/terminal/terminalStore.svelte';
import { openBrowserPaneForTest } from '../../test/helpers/browserPane';

describe('accelerators from the native page view', () => {
  beforeEach(() => openBrowserPaneForTest('thread-browser', 'mount-1'));

  it('focuses the companion and replays the chord as a window keydown', () => {
    focusPane('main');
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'w', meta: true },
      });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(getFocusedPaneId()).toBe('browser-main');
    expect(seen).toHaveLength(1);
    expect(seen[0].key).toBe('w');
    expect(seen[0].metaKey).toBe(true);
    expect(seen[0].ctrlKey).toBe(false);
  });

  // DOM focus left in the SPA (a composer or terminal caret) is where the
  // user returns to; a replayed chord that moves no focus leaves it there.
  it('leaves DOM focus where it was across a replayed chord', () => {
    const composer = document.createElement('textarea');
    document.body.appendChild(composer);
    composer.focus();
    const blurred = vi.fn();
    composer.addEventListener('blur', blurred);
    let focusedAtReplay: Element | null = null;
    const listener = () => { focusedAtReplay = document.activeElement; };
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'f', ctrl: true },
      });
      expect(focusedAtReplay).toBe(composer);
      expect(document.activeElement).toBe(composer);
      expect(blurred).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('keydown', listener);
      composer.remove();
    }
  });

  // The engine hands keyboard focus back to the SPA when it claims a chord,
  // so a terminal caret the SPA kept may be focused again by the time the
  // chord arrives: the chord was typed in the page, not the terminal.
  it('resolves a replayed chord outside a terminal the SPA kept focus in', () => {
    notifyTerminalFocus('main', true);
    const hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    let during: boolean | null = null;
    const listener = () => { during = makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus; };
    window.addEventListener('keydown', listener);
    try {
      expect(makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus).toBe(true);
      applyBrowserCompanionState({
        kind: 'accelerator',
        threadId: 'thread-browser',
        mount: 'mount-1',
        accelerator: { key: 'w', ctrl: true },
      });
      expect(during).toBe(false);
      expect(makeCommandContext(getPane('main') ?? null, {}).flags.terminalFocus).toBe(true);
    } finally {
      window.removeEventListener('keydown', listener);
      hasFocus.mockRestore();
      notifyTerminalFocus('main', false);
    }
  });

  // Every host client receives the event; a client whose pane mount did not
  // show the page, or that holds none, replays nothing.
  it.each([
    ['another client\'s mount', 'mount-2'],
    ['no mount named', undefined],
  ])('does not replay a chord routed to %s', (_name, mount) => {
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount, accelerator: { key: 'w', ctrl: true } });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
    expect(getFocusedPaneId()).not.toBe('browser-main');
  });

  // A chord pressed in the page arrives after the user moved on: it acts in
  // the pane it was pressed in or nowhere, never in the pane that covers it.
  it.each([
    ['another pane was promoted over it', () => promotePane('main')],
    ['the compact list screen covers it', () => setCompactLayoutForTest(true)],
  ])('drops a chord whose pane is no longer the keyboard\'s target because %s', (_name, cover) => {
    focusPane('browser-main');
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      cover();
      focusPane('main');
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1', accelerator: { key: 'w', ctrl: true } });
      expect(seen).toEqual([]);
      expect(getFocusedPaneId()).toBe('main');
    } finally {
      window.removeEventListener('keydown', listener);
      demotePane();
      setCompactLayoutForTest(false);
    }
  });

  it('replays a chord in the browser pane promoted over the strip', () => {
    expect(promotePane('browser-main')).toBe(true);
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1', accelerator: { key: 'w', ctrl: true } });
    } finally {
      window.removeEventListener('keydown', listener);
      demotePane();
    }
    expect(seen).toHaveLength(1);
  });

  it('does not replay a chord in a client with no pane mounted', () => {
    resetBrowserCompanionForTest();
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1', accelerator: { key: 'w', ctrl: true } });
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', accelerator: { key: 'w', ctrl: true } });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
  });

  it('is inert without a chord', () => {
    const seen: KeyboardEvent[] = [];
    const listener = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', listener);
    try {
      applyBrowserCompanionState({ kind: 'accelerator', threadId: 'thread-browser', mount: 'mount-1' });
    } finally {
      window.removeEventListener('keydown', listener);
    }
    expect(seen).toEqual([]);
  });
});
