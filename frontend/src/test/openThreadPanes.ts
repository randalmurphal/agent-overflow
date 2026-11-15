// The panes a happy-dom test opened, recorded by the thread-pane-tracking
// transform in vitest.config.ts. An open pane keeps its replica write-back
// timer, which would fire into a later test's replica; the shared teardown
// (setup.ts) clears every recorded pane, as closing a pane does in the app.

export const OPEN_THREAD_PANES_KEY = 'agent-overflow.test.openThreadPanes';

/** Clear every pane created since the last call. */
export function clearOpenThreadPanes(): void {
  const open = (globalThis as Record<symbol, Array<{ clear(): void }> | undefined>)[Symbol.for(OPEN_THREAD_PANES_KEY)];
  for (const pane of open?.splice(0) ?? []) pane.clear();
}
