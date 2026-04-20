import type { Item, Thread } from '../types/models';
import type { ApprovalRequest, ContextWindow, ProviderStatusEvent } from '../types/events';
import type { ChannelMessage } from '../types/discussion';
import type { DesignArtifact, DesignOptionsRequest, DesignViewport } from '../types/design';
import { ListItems, SwitchThread } from './bindings';
import { addToast } from './toast.svelte';
import { createDiffPanelState, type DiffPanelState } from './diffPanel.svelte';

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, unified timeline items, approvals,
 * context/banner state, and mode-specific UI. Components receive a
 * ThreadPane as a prop.
 */
export function createThreadPane() {
  let thread: Thread | null = $state(null);
  let items: Item[] = $state([]);
  let pendingApprovals: ApprovalRequest[] = $state([]);
  let contextWindow: ContextWindow | null = $state(null);
  let providerBanner: ProviderStatusEvent | null = $state(null);
  let error: string | null = $state(null);
  let loading: boolean = $state(false);
  let showTerminal: boolean = $state(false);
  // Diff panel is per-pane; created once and reset on thread switch so its
  // caches don't leak between threads.
  const diffPanel: DiffPanelState = createDiffPanelState();

  // Channel state (only populated for discussion threads).
  let channelMessages: ChannelMessage[] = $state([]);
  let channelStatus: 'open' | 'concluded' | 'closed' | null = $state(null);

  // Design-mode state (only populated when thread.mode === 'design').
  // designArtifacts is the render+option history for the thread.
  // activeArtifactId is what the preview panel is displaying — null = show latest.
  // pendingDesignOptions is populated when an agent has blocked on present_options.
  // designViewport drives the iframe width toggle.
  let designArtifacts: DesignArtifact[] = $state([]);
  let activeArtifactId: string | null = $state(null);
  let pendingDesignOptions: DesignOptionsRequest | null = $state(null);
  let designViewport: DesignViewport = $state('desktop');

  // PlanSidebar toggle state. Per-pane so each pane can open/close its
  // own sidebar independently. Reset on thread switch so a new thread
  // never "remembers" whether the prior thread had the sidebar open.
  let showPlanSidebar: boolean = $state(false);

  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow ListItems from thread A cannot clobber thread B's items
   * when the user flips between them quickly.
   */
  let switchGeneration = 0;

  function seedContextWindow(nextThread: Thread | null): ContextWindow | null {
    const raw = nextThread?.lastTokenUsage?.trim();
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as {
        usedTokens?: number;
        maxTokens?: number;
        contextPercent?: number;
      };
      if (typeof parsed.usedTokens !== 'number') return null;
      return {
        usedTokens: parsed.usedTokens,
        maxTokens: parsed.maxTokens,
        usedPercentage: parsed.contextPercent,
      };
    } catch {
      return null;
    }
  }

  return {
    // --- Getters (reactive reads) ---
    get thread() { return thread; },
    get threadId() { return thread?.id ?? null; },
    get items() { return items; },
    get pendingApprovals() { return pendingApprovals; },
    get contextWindow() { return contextWindow; },
    get providerBanner() { return providerBanner; },
    get error() { return error; },
    get loading() { return loading; },
    get showTerminal() { return showTerminal; },
    get diffPanel() { return diffPanel; },
    /**
     * True while a provider turn is in-flight for the current pane. The
     * composer uses this to block sends and surface the interrupt affordance.
     * Pending approvals count as active; backgrounded tools do not.
     */
    get isTurnActive() {
      return (
        items.some((item) =>
          ((item.kind === 'assistant_text' || item.kind === 'thinking') && item.status === 'streaming')
          || (item.kind === 'tool_call' && item.status === 'running' && !item.isBackground),
        )
        || pendingApprovals.length > 0
      );
    },
    get channelMessages() { return channelMessages; },
    get channelStatus() { return channelStatus; },
    get designArtifacts() { return designArtifacts; },
    get activeArtifactId() { return activeArtifactId; },
    get pendingDesignOptions() { return pendingDesignOptions; },
    get designViewport() { return designViewport; },
    get showPlanSidebar() { return showPlanSidebar; },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      pendingApprovals = [];
      contextWindow = seedContextWindow(newThread);
      providerBanner = null;
      error = null;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      // Every drawer / pane-scoped UI flag should reset so switching threads
      // never "remembers" the previous thread's open drawers. diffPanel owns
      // its own reset via clearForThread (which closes the panel); showTerminal
      // is reset here so opening the terminal on thread A does not spill over
      // into thread B.
      showTerminal = false;
      // Plan-sidebar UI is pane-scoped too: never carry its open state across
      // threads.
      showPlanSidebar = false;
      diffPanel.clearForThread();
      loading = true;
      items = [];

      thread = newThread;
      // Capture the switch generation at the top so every await below can bail
      // out if the user has already switched away (or back).
      const gen = ++switchGeneration;

      // Notify the backend so it can auto-start sessions for threads with session_ref.
      try {
        await SwitchThread(newThread.id);
      } catch (err) {
        console.error('Failed to notify backend of thread switch:', err);
        addToast('warning', 'Backend was not notified of thread switch');
      }
      if (gen !== switchGeneration) return;

      try {
        const loaded = await ListItems(newThread.id) as Item[];
        if (gen !== switchGeneration) return;
        items = loaded;
      } catch (err) {
        if (gen !== switchGeneration) return;
        console.error('Failed to load items:', err);
        items = [];
        error = `Failed to load thread items: ${err}`;
        addToast('error', 'Failed to load thread items');
      }

      if (gen !== switchGeneration) return;
      loading = false;
    },

    clear(): void {
      thread = null;
      items = [];
      pendingApprovals = [];
      contextWindow = null;
      providerBanner = null;
      error = null;
      loading = false;
      showTerminal = false;
      showPlanSidebar = false;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      diffPanel.clearForThread();
      // Invalidate any in-flight switchThread so its late resolutions can't
      // repopulate the pane we just cleared.
      switchGeneration++;
    },

    // --- Mutations (called by event router) ---

    addApproval(approval: ApprovalRequest): void {
      pendingApprovals = [...pendingApprovals, approval];
    },

    removeApproval(requestId: string): void {
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    /**
     * Merge a single Item from a `provider:item_upsert` event into the
     * timeline. New ids append in (turnIndex, itemIndex) order; existing
     * ids replace in place so the row's status/summary/payload_id can
     * mutate without losing position. The backend is authoritative for
     * ordering — we never reshuffle by anything other than those two
     * fields, so a tool_call row stays exactly where it was inserted.
     */
    upsertItem(item: Item): void {
      const idx = items.findIndex((existing) => existing.id === item.id);
      if (idx >= 0) {
        const next = items.slice();
        next[idx] = item;
        items = next;
        return;
      }
      // Find the insertion point that preserves (turnIndex, itemIndex).
      // Linear scan is fine — typical pane carries < 1k items and the
      // upsert volume during a turn is bounded by how many tools the
      // agent runs.
      let insertAt = items.length;
      for (let i = 0; i < items.length; i++) {
        const cur = items[i];
        if (
          cur.turnIndex > item.turnIndex
          || (cur.turnIndex === item.turnIndex && cur.itemIndex > item.itemIndex)
        ) {
          insertAt = i;
          break;
        }
      }
      const next = items.slice();
      next.splice(insertAt, 0, item);
      items = next;
    },

    setError(message: string | null): void {
      error = message;
    },

    clearError(): void {
      error = null;
    },

    setContextWindow(data: ContextWindow): void {
      contextWindow = data;
    },

    clearContextWindow(): void {
      contextWindow = null;
    },

    setProviderBanner(status: ProviderStatusEvent | null): void {
      providerBanner = status;
    },

    replaceThread(nextThread: Thread): void {
      thread = nextThread;
    },

    toggleTerminal(): void {
      showTerminal = !showTerminal;
    },

    setShowTerminal(value: boolean): void {
      showTerminal = value;
    },

    toggleDiffPanel(): void {
      diffPanel.toggle();
    },

    togglePlanSidebar(): void {
      showPlanSidebar = !showPlanSidebar;
    },

    setShowPlanSidebar(value: boolean): void {
      showPlanSidebar = value;
    },

    setDiffPanelOpen(value: boolean): void {
      if (value) {
        diffPanel.open_();
      } else {
        diffPanel.close();
      }
    },

    /**
     * Merge channel messages into local state, de-duplicating by sequence.
     * Expected to be called with `afterSeq` set to the highest sequence we've
     * seen, so most calls append a small number of rows.
     */
    mergeChannelMessages(incoming: ChannelMessage[]): void {
      if (!incoming || incoming.length === 0) return;
      const seen = new Set(channelMessages.map((m) => m.sequence));
      const next = channelMessages.slice();
      for (const msg of incoming) {
        if (!seen.has(msg.sequence)) {
          next.push(msg);
          seen.add(msg.sequence);
        }
      }
      next.sort((a, b) => a.sequence - b.sequence);
      channelMessages = next;
    },

    setChannelStatus(status: 'open' | 'concluded' | 'closed' | null): void {
      channelStatus = status;
    },

    clearChannel(): void {
      channelMessages = [];
      channelStatus = null;
    },

    // --- Design-mode mutations ---

    /**
     * Replace the artifact history in one shot. Used when the panel first
     * mounts and hydrates from ListDesignArtifacts.
     */
    setDesignArtifacts(artifacts: DesignArtifact[]): void {
      designArtifacts = [...artifacts];
    },

    /**
     * Append an artifact. De-dupes by id so idempotent event replays don't
     * double-insert. New artifacts become the implicit active one (unless the
     * user has pinned a different artifact via setActiveArtifact).
     */
    appendDesignArtifact(artifact: DesignArtifact): void {
      const exists = designArtifacts.some((a) => a.id === artifact.id);
      if (exists) return;
      designArtifacts = [...designArtifacts, artifact];
    },

    setActiveArtifact(artifactId: string | null): void {
      activeArtifactId = artifactId;
    },

    setDesignOptions(request: DesignOptionsRequest | null): void {
      pendingDesignOptions = request;
    },

    clearDesignOptions(): void {
      pendingDesignOptions = null;
    },

    setDesignViewport(viewport: DesignViewport): void {
      designViewport = viewport;
    },

    clearDesign(): void {
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
