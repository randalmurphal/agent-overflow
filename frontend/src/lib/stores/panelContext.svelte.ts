import type { Item, Thread } from '../types/models';
import type { ApprovalRequest } from '../types/events';
import type { ActiveOptionSet, DesignViewport } from '../types/design';
import { mountThreadInPane, syncThread } from './panes.svelte';
import { agentStateForPane } from './agentPane.svelte';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { SettledTurn } from './threadTurnProjection';
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
 *
 * The AGENT panel is the one body that legitimately reads timeline state
 * (`items`, `timelineRevision`, …): it renders the source thread's own
 * transcript, filtered to a subagent's scope, so those reads are its subject
 * matter rather than an accidental coupling. That is exactly why they are
 * enumerated here one by one instead of handing the body `pane` — no OTHER
 * panel kind should touch them, and adding one to this list is a decision
 * someone has to make on purpose.
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
  /** The source pane's loaded timeline window. Agent panel only. */
  readonly items: Item[];
  /** Bumped on every structural timeline change — the cutoff a projection
   *  over `items` recomputes on. Agent panel only. */
  readonly timelineRevision: number;
  /** Row lookup by id, for resolving a scope/breadcrumb to its launch row.
   *  Agent panel only. */
  getItemById(itemId: string): Item | undefined;
  /** Hydrate the evicted child transcript under a subagent launch anchor
   *  (`threadSubagentMemory.hydrateChildren`). Scoping the pane to a node
   *  whose children were evicted is exactly the case that needs it. Agent
   *  panel only. */
  ensureSubagentChildren(rootItemId: string): Promise<boolean>;
  /** Pending approval requests, so a scoped node can show its own.
   *  Agent panel only. */
  readonly pendingApprovals: ApprovalRequest[];
  /** The pane's activity-run registry — run identity and collapse state are
   *  per PANE, so the agent body must render from the same one the chat
   *  surface does rather than minting a second. Agent panel only. */
  readonly activityRuns: ThreadActivityRuns;
  /** Most recent settled turn. Agent panel only. */
  readonly latestSettledTurn: SettledTurn | null;
  /** Whether the source pane has something to compose against — the agent
   *  pane's composer shell is non-interactive but still renders enabled or
   *  not from the same fact. Agent panel only. */
  readonly canCompose: boolean;
  /** Publish a scroll-to-item intent on the SOURCE pane's timeline (the
   *  background-section click path, spec Q8). Agent panel only. */
  requestScrollToItem(itemId: string): void;
  /** Descend into a child node from inside the agent pane: swaps the scope
   *  and pushes a breadcrumb hop (spec Q4b — no stacking). No-op when the
   *  pane has no thread. */
  openAgentScope(itemId: string, label: string): void;
  /** Close the agent companion for this source pane and drop its scope.
   *  The body calls this when the scoped row resolves to nothing. */
  closeAgentPane(): void;
  /** Close this panel (X button, ESC, programmatic). Generic across panel
   *  kinds — injected by the owning shell. */
  close(): void;
  /** Sync an updated thread to every UI surface holding it (the owning
   *  pane plus the global threads registry). Use after a panel action
   *  mutates the thread server-side. */
  replaceThread(thread: Thread): void;
  /** Show another thread after a panel action creates one. Routes through the
   *  pane-mount chokepoint, so it reveals an already-open thread rather than
   *  mounting a second copy of it — and the pane's new contents are persisted
   *  with the layout. */
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
    get items() { return pane.items; },
    get timelineRevision() { return pane.timelineRevision; },
    get pendingApprovals() { return pane.pendingApprovals; },
    get activityRuns() { return pane.activityRuns; },
    get latestSettledTurn() { return pane.latestSettledTurn; },
    get canCompose() { return pane.canCompose; },
    getItemById: (itemId) => pane.getItemById(itemId),
    ensureSubagentChildren: (rootItemId) => pane.ensureSubagentChildren(rootItemId),
    requestScrollToItem: (itemId) => pane.requestScrollToItem(itemId),
    openAgentScope: (itemId, label) => {
      const threadId = pane.threadId;
      if (!threadId || !itemId) return;
      agentStateForPane(pane.paneId, threadId).pushScope(itemId, label);
    },
    closeAgentPane: () => pane.closeAgentPane(),
    close,
    replaceThread: syncThread,
    switchThread: async (thread) => { await mountThreadInPane(thread, pane); },
    setDesignViewport: (viewport) => pane.setDesignViewport(viewport),
    setActiveOptionSet: (set) => pane.setActiveOptionSet(set),
    refreshDesignOptions: (threadId) => pane.applyDesignOptionsUpdate(threadId, ''),
  };
}
