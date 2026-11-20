import { getContext, setContext } from 'svelte';

// The browser pane's thread, for controls inside its chrome that act on the
// thread's browser without every chrome component passing it down.
const KEY = Symbol('browser-pane-thread');

/** Provides the pane's thread to its chrome; call during BrowserPane's init. */
export function provideBrowserPaneThread(thread: () => string | null): void {
  setContext(KEY, thread);
}

/** The enclosing browser pane's thread, null outside one. */
export function browserPaneThread(): () => string | null {
  return getContext<(() => string | null) | undefined>(KEY) ?? (() => null);
}
