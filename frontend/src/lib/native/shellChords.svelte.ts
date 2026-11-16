// The SPA's half of the native shell's reload chord. The desktop window binds
// Ctrl/Cmd+R to reloading the app before the page sees the key; while the
// browser pane is the focused pane the chord means "reload the page" instead,
// as it does inside the native page. The shell routes it here when told the
// pane is focused, and this module runs `browser.reload` or hands the chord
// back. The Go half is internal/uikeys/pagechords.go; the names mirror it.

import { runCommand, type CommandContext } from '../stores/commandRegistry.svelte';
import { isBrowserPaneFocused } from '../stores/browserCommands.svelte';

const MESSAGE_PREFIX = 'ao:shell:';
const CAPABILITY_GLOBAL = '__aoShellChords';
const READY_EVENT = 'ao-shell-chords';
const HANDLER_GLOBAL = '__aoShellChord';
export const RELOAD_APP_MESSAGE = MESSAGE_PREFIX + 'reload-app';

export function focusMessage(focused: boolean): string {
  return `${MESSAGE_PREFIX}browser-pane-focus:${focused ? 1 : 0}`;
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
 * Reports the browser pane's focus to the shell and takes the chords it
 * routes back. `context` is the per-keypress command context, so a routed
 * chord is live exactly when the same chord pressed in the page would be.
 * The result removes both.
 */
export function installShellChords(context: () => CommandContext): () => void {
  const w = shellWindow();
  w[HANDLER_GLOBAL] = (name: string) => {
    if (name !== 'reload') return;
    if (runCommand('browser.reload', context())) return;
    // Not ours after all (another pane took focus, or an overlay is open):
    // the shell reloads the app, as the chord means there.
    post(RELOAD_APP_MESSAGE);
  };

  let posted: boolean | null = null;
  const report = (focused: boolean, force: boolean) => {
    if (!force && posted === focused) return;
    if (post(focusMessage(focused))) posted = focused;
  };
  const stopEffect = $effect.root(() => {
    $effect(() => {
      report(isBrowserPaneFocused(), false);
    });
  });
  // The shell advertises itself per document, possibly after the effect ran.
  const onReady = () => report(isBrowserPaneFocused(), true);
  window.addEventListener(READY_EVENT, onReady);

  return () => {
    stopEffect();
    window.removeEventListener(READY_EVENT, onReady);
    if (w[HANDLER_GLOBAL]) delete w[HANDLER_GLOBAL];
    report(false, false);
  };
}
