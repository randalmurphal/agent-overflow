// The SPA's half of the native shell's reload chord. The desktop window binds
// Ctrl/Cmd+R to reloading the app before the page sees the key. While the
// browser pane is the focused pane and the effective keybindings run a
// command for that chord there, the SPA asks the shell to hand the chord
// over instead, and replays it as the keydown the window withheld: it goes
// through the one keybinding dispatcher, as a chord pressed in the native
// page does (browserCompanion.svelte.ts replayBrowserAccelerator). A chord
// nothing runs goes back to the shell, which reloads the app. The Go half
// is internal/uikeys/pagechords.go; the names mirror it.

import type { CommandContext } from '../stores/commandRegistry.svelte';
import { isBrowserPaneFocused } from '../stores/browserCommands.svelte';
import { keyWouldDispatch } from '../stores/keybindings.svelte';
import { isMacPlatform } from '../utils/platform';

const MESSAGE_PREFIX = 'ao:shell:';
const CAPABILITY_GLOBAL = '__aoShellChords';
const READY_EVENT = 'ao-shell-chords';
const HANDLER_GLOBAL = '__aoShellChord';
export const RELOAD_APP_MESSAGE = MESSAGE_PREFIX + 'reload-app';

export function routeMessage(route: boolean): string {
  return `${MESSAGE_PREFIX}route-reload:${route ? 1 : 0}`;
}

interface ShellWindow {
  [CAPABILITY_GLOBAL]?: unknown;
  [HANDLER_GLOBAL]?: (name: string) => void;
  chrome?: { webview?: { postMessage?: (message: string) => void } };
  webkit?: { messageHandlers?: { external?: { postMessage?: (message: string) => void } } };
}

function shellWindow(): Window & ShellWindow {
  return window as Window & ShellWindow;
}

/** The shell's reload chord (Wails `CmdOrCtrl+r`) as the keydown it withheld. */
function reloadChord(): KeyboardEvent {
  const mac = isMacPlatform();
  return new KeyboardEvent('keydown', { key: 'r', metaKey: mac, ctrlKey: !mac, bubbles: true, cancelable: true });
}

/** Posts to the shell when it routes chords; false when it does not. */
function post(message: string): boolean {
  const w = shellWindow();
  if (w[CAPABILITY_GLOBAL] !== 1) return false;
  // Each bridge method needs its owning receiver.
  const webview = w.chrome?.webview;
  if (typeof webview?.postMessage === 'function') {
    webview.postMessage(message);
    return true;
  }
  const external = w.webkit?.messageHandlers?.external;
  if (typeof external?.postMessage === 'function') {
    external.postMessage(message);
    return true;
  }
  return false;
}

/**
 * Tells the shell when to hand over its reload chord and replays the chord
 * it hands over. `context` is the per-keypress command context, the one the
 * replayed keydown is dispatched with. The result removes both.
 */
export function installShellChords(context: () => CommandContext): () => void {
  const w = shellWindow();
  w[HANDLER_GLOBAL] = (name: string) => {
    if (name !== 'reload') return;
    const event = reloadChord();
    (document.activeElement ?? document.body).dispatchEvent(event);
    // Nothing ran it (focus or bindings changed since the last report): the
    // shell reloads the app, as the chord means there.
    if (!event.defaultPrevented) post(RELOAD_APP_MESSAGE);
  };

  let posted: boolean | null = null;
  const report = (route: boolean, force: boolean) => {
    if (!force && posted === route) return;
    if (post(routeMessage(route))) posted = route;
  };
  const wanted = () => isBrowserPaneFocused() && keyWouldDispatch(reloadChord(), context());
  const stopEffect = $effect.root(() => {
    $effect(() => {
      report(wanted(), false);
    });
  });
  // The shell advertises itself per document, possibly after the effect ran.
  const onReady = () => report(wanted(), true);
  window.addEventListener(READY_EVENT, onReady);

  return () => {
    stopEffect();
    window.removeEventListener(READY_EVENT, onReady);
    if (w[HANDLER_GLOBAL]) delete w[HANDLER_GLOBAL];
    report(false, false);
  };
}
