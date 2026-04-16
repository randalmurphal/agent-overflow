// Fake implementation of `@wailsio/runtime` for tests.
//
// The real module ships a Wails-specific IPC channel that only works inside a
// Wails webview. Tests run in happy-dom and never initialise that channel, so
// we replace it with a synchronous pub-sub registry that tests can drive
// directly via `emitWailsEvent()`.

type Handler = (ev: { name: string; data: unknown }) => void;

const listeners: Map<string, Set<Handler>> = new Map();

export const Events = {
  /**
   * Register a handler for a named event. Returns an unsubscribe function
   * matching the real runtime's contract.
   */
  On(name: string, handler: Handler): () => void {
    let set = listeners.get(name);
    if (!set) {
      set = new Set();
      listeners.set(name, set);
    }
    set.add(handler);
    return () => {
      const current = listeners.get(name);
      if (!current) return;
      current.delete(handler);
      if (current.size === 0) listeners.delete(name);
    };
  },

  /**
   * Emit an event into the mock bus. Not something the real runtime exposes
   * this way, but tests synthesise events through `emitWailsEvent()`.
   */
  Emit(_event: { name: string; data: unknown }): void {
    // No-op in tests unless a specific suite wants to mock-track emits.
  },
};

/**
 * Synchronously invoke every registered handler for `name`. Use this from
 * tests to drive the event router as if a real provider event arrived.
 */
export function emitWailsEvent(name: string, data: unknown): void {
  const set = listeners.get(name);
  if (!set) return;
  // Copy to avoid mutation-during-iteration if handlers unsubscribe.
  for (const handler of [...set]) {
    handler({ name, data });
  }
}

/**
 * Count of handlers currently attached to `name`. Lets tests assert that
 * cleanup functions actually unsubscribe.
 */
export function wailsListenerCount(name: string): number {
  return listeners.get(name)?.size ?? 0;
}

/**
 * Reset all listeners between tests so state doesn't leak.
 */
export function resetWailsMocks(): void {
  listeners.clear();
}
