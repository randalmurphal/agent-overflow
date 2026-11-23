// The panes a happy-dom test opened, recorded by the thread-pane-tracking
// transform (config/threadPaneTracker.ts). An open pane keeps its replica write-back
// timer, which would fire into a later test's replica; the between-test
// steps (config/testResets.ts) clear every recorded pane, as closing a pane
// does in the app.

import { OPEN_THREAD_PANES_KEY } from './config/threadPaneTracker';

/** Clear every pane created since the last call. */
export function clearOpenThreadPanes(): void {
  const open = (globalThis as Record<symbol, Array<{ clear(): void }> | undefined>)[Symbol.for(OPEN_THREAD_PANES_KEY)];
  for (const pane of open?.splice(0) ?? []) pane.clear();
}
