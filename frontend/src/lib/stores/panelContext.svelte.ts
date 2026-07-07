import type { Thread } from '../types/models';
import type { ActiveOptionSet, DesignViewport } from '../types/design';
import { syncThread } from './panes.svelte';
import type { ThreadPane } from './thread.svelte';

/**
 * Narrow projection of a ThreadPane handed to panel bodies. Bodies receive
 * this instead of `pane: ThreadPane` so they cannot accidentally couple to
 * chat-only state (`pane.items`, `pane.timelineRevision`, streaming flags) —
 * coupling that, in PlanSidebar, was the structural cause of body-blank
 * flicker during chat streaming.
 *
 * Adding a new panel kind that needs row registries (expansion state,
 * attachment cache, subagent group flags) should EXTEND this interface with
 * the specific accessors it needs, not widen back to `pane`.
 */
export interface PanelContext {
  /** Thread this panel belongs to. May be null when the pane has no thread
   *  loaded — bodies should treat null as "nothing to render" rather than
   *  rendering a degraded surface. The shell remounts the body on thread
   *  switch via its `{#key}` boundary. */
  threadId: string | null;
  /** Current thread object for panel actions that need thread metadata. */
  thread: Thread | null;
  /** Stable identifier for the owning pane. Plumbed for multi-pane surfaces
   *  where each pane has its own panel. */
  paneId: string;
  /** Workspace root for resolving relative paths in panel content. */
  workspacePath: string | undefined;
  /** Design preview viewport selected for this pane. */
  designViewport: DesignViewport;
  /** Active design option set, when the agent has emitted choices. */
  activeOptionSet: ActiveOptionSet | null;
  /** Close this panel (X button, ESC, programmatic). Generic across panel
   *  kinds — injected by the owning shell. */
  close(): void;
  /** Sync an updated thread to every UI surface holding it (the owning
   *  pane plus the global threads registry). Use after a panel action
   *  mutates the thread server-side. */
  replaceThread(thread: Thread): void;
  /** Switch this pane to another thread after a panel action creates one. */
  switchThread(thread: Thread): Promise<void>;
  /** Update the design preview viewport. */
  setDesignViewport(viewport: DesignViewport): void;
  /** Activate or clear the design option set. */
  setActiveOptionSet(set: ActiveOptionSet | null): void;
  /** Rehydrate design options from the backend workdir. */
  refreshDesignOptions(threadId: string): Promise<void>;
}

/**
 * Builds a stable PanelContext for the given pane.
 *
 * `threadId` and `workspacePath` are exposed as GETTERS so the returned
 * object's identity is stable across `pane.thread` reassignments (which
 * happen on every turn_started, turn_completed, usage event, and
 * thread:updated event — many per chat message). Reactive consumers reading
 * `ctx.threadId` still pick up changes via the getter.
 *
 * Without getter-based stability, companion pane shells would re-create the
 * ctx object on every pane.thread reassignment, propagating prop-identity
 * churn into PlanSidebar and visibly flickering that surface.
 */
export function makePanelContext(pane: ThreadPane, close: () => void): PanelContext {
  return {
    paneId: pane.paneId,
    get threadId() { return pane.threadId; },
    get thread() { return pane.thread; },
    get workspacePath() { return pane.thread?.workspacePath; },
    get designViewport() { return pane.designViewport; },
    get activeOptionSet() { return pane.activeOptionSet; },
    close,
    replaceThread: syncThread,
    switchThread: (thread) => pane.switchThread(thread),
    setDesignViewport: (viewport) => pane.setDesignViewport(viewport),
    setActiveOptionSet: (set) => pane.setActiveOptionSet(set),
    refreshDesignOptions: (threadId) => pane.applyDesignOptionsUpdate(threadId, ''),
  };
}
