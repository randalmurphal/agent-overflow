import type { Item, PayloadMeta, Thread } from '../types/models';
import type { ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage, ToolProgressMeta } from '../types/events';
import type { ChannelMessage } from '../types/discussion';
import type { DesignArtifact, DesignOptionsRequest, DesignViewport } from '../types/design';
import { ListItems, ListPayloadMetas, SwitchThread } from './bindings';
import { addToast } from './toast.svelte';
import { createDiffPanelState, type DiffPanelState } from './diffPanel.svelte';

/**
 * Hard cap on payloadMetas entries kept alive per pane. Long threads can
 * amass thousands of payload rows (command outputs, diffs, attachments,
 * etc.); at a certain point the map stops helping and starts eating RAM.
 * LRU eviction keeps the recently-used entries; the backend can always
 * re-fetch any evicted id on demand via ListPayloadMetas/GetPayloadData.
 */
export const PAYLOAD_META_LIMIT = 500;

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, items, streaming state, approvals, etc.
 * Components receive a ThreadPane as a prop.
 */
export function createThreadPane() {
  let thread: Thread | null = $state(null);
  let items: Item[] = $state([]);
  let payloadMetas: Map<string, PayloadMeta> = $state(new Map());
  let streamingContent: string = $state('');
  let activeToolCalls: Map<string, unknown> = $state(new Map());
  let pendingApprovals: ApprovalRequest[] = $state([]);
  let backgroundTasks: Map<string, unknown> = $state(new Map());
  // 'idle' is the neutral boot/in-between state. The ProviderStatusBanner
  // only surfaces 'disconnected' / 'error' / 'retrying', so a fresh pane
  // starts quiet and only lights up once the backend emits a terminal
  // session_status. Previously this defaulted to 'disconnected', which
  // flashed the "Session disconnected" banner under every brand-new thread
  // until an init event arrived — confusing for Claude, where init only
  // ships after the first Send.
  let sessionStatus: string = $state('idle');
  let tokenUsage: TokenUsage | null = $state(null);
  let contextWindow: ContextWindow | null = $state(null);
  let rateLimits: RateLimitEntry[] = $state([]);
  let sessionApprovedTools: Set<string> = $state(new Set());
  let error: string | null = $state(null);
  let loading: boolean = $state(false);
  let pendingMessage: string | null = $state(null);
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

  // Streaming in-progress plan (Codex `turn/plan/updated`). The
  // finalized plan surfaces via the timeline as a ProposedPlanCard when
  // the item completes; this field holds the opaque incremental payload
  // the PlanSidebar renders while a plan is still being proposed. Reset
  // on turn complete and on thread switch.
  let pendingPlanUpdate: unknown = $state(null);

  // PlanSidebar toggle state. Per-pane so each pane can open/close its
  // own sidebar independently. Reset on thread switch so a new thread
  // never "remembers" whether the prior thread had the sidebar open.
  let showPlanSidebar: boolean = $state(false);

  // Id of the proposed-plan item the user has actively dismissed from the
  // PlanFollowUpBanner. While this is set and matches the latest item in
  // the pane, the banner stays hidden. Reset on thread switch (every new
  // thread starts fresh) and on finalizeTurn (a completed turn produces
  // fresh items, so a previously-dismissed id is no longer the latest).
  let dismissedPlanItemId: string | null = $state(null);

  /**
   * Generation counter for finalizeTurn. Incremented each time finalizeTurn
   * starts so that stale async DB responses don't overwrite newer state.
   */
  let turnGeneration = 0;

  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow ListItems/ListPayloadMetas from thread A cannot clobber
   * thread B's items when the user flips between them quickly.
   */
  let switchGeneration = 0;

  return {
    // --- Getters (reactive reads) ---
    get thread() { return thread; },
    get threadId() { return thread?.id ?? null; },
    get items() { return items; },
    get payloadMetas() { return payloadMetas; },
    get streamingContent() { return streamingContent; },
    get activeToolCalls() { return activeToolCalls; },
    get pendingApprovals() { return pendingApprovals; },
    get backgroundTasks() { return backgroundTasks; },
    get sessionStatus() { return sessionStatus; },
    get tokenUsage() { return tokenUsage; },
    get contextWindow() { return contextWindow; },
    get rateLimits() { return rateLimits; },
    get error() { return error; },
    get loading() { return loading; },
    get pendingMessage() { return pendingMessage; },
    get showTerminal() { return showTerminal; },
    get diffPanel() { return diffPanel; },
    /**
     * True while a provider turn is in-flight for the current pane. Mirrors the
     * mid-turn guard: the composer uses this to block sends, show the Interrupt
     * affordance, and suppress Enter until the user interrupts or the turn
     * completes. The condition is intentionally the union of every streaming
     * indicator so an in-flight turn with no text yet (just queued tool calls
     * or a pending optimistic user message) is still detected.
     */
    get isTurnActive() {
      return (
        streamingContent.length > 0
        || activeToolCalls.size > 0
        || pendingMessage !== null
      );
    },
    get channelMessages() { return channelMessages; },
    get channelStatus() { return channelStatus; },
    get designArtifacts() { return designArtifacts; },
    get activeArtifactId() { return activeArtifactId; },
    get pendingDesignOptions() { return pendingDesignOptions; },
    get designViewport() { return designViewport; },
    get pendingPlanUpdate() { return pendingPlanUpdate; },
    get showPlanSidebar() { return showPlanSidebar; },
    get dismissedPlanItemId() { return dismissedPlanItemId; },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      streamingContent = '';
      activeToolCalls = new Map();
      pendingApprovals = [];
      backgroundTasks = new Map();
      tokenUsage = null;
      contextWindow = null;
      rateLimits = [];
      sessionApprovedTools = new Set();
      sessionStatus = 'idle';
      error = null;
      pendingMessage = null;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      pendingPlanUpdate = null;
      // Every drawer / pane-scoped UI flag should reset so switching threads
      // never "remembers" the previous thread's open drawers. diffPanel owns
      // its own reset via clearForThread (which closes the panel); showTerminal
      // is reset here so opening the terminal on thread A does not spill over
      // into thread B.
      showTerminal = false;
      // Plan-sidebar UI is pane-scoped too: never carry its open state across
      // threads. Dismissed-plan id belongs to the previous thread's items, so
      // it must clear along with the thread.
      showPlanSidebar = false;
      dismissedPlanItemId = null;
      diffPanel.clearForThread();
      loading = true;
      items = [];
      payloadMetas = new Map();

      thread = newThread;
      // Bump generation so any in-flight finalizeTurn from prior thread is discarded.
      turnGeneration++;
      // Capture the switch generation at the top so every await below can bail
      // out if the user has already switched away (or back). Without this, a
      // slow ListItems from thread A can race with a fresh ListItems from
      // thread B and overwrite B's rows with A's.
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

      try {
        const metas = await ListPayloadMetas(newThread.id) as PayloadMeta[];
        if (gen !== switchGeneration) return;
        // Backend may return the full history; we keep only the most recent
        // PAYLOAD_META_LIMIT entries so a freshly-hydrated long thread does
        // not start over the LRU cap.
        const source = metas ?? [];
        const sliced = source.length > PAYLOAD_META_LIMIT
          ? source.slice(source.length - PAYLOAD_META_LIMIT)
          : source;
        payloadMetas = new Map(sliced.map((m: PayloadMeta) => [m.id, m]));
      } catch (err) {
        if (gen !== switchGeneration) return;
        console.error('Failed to load payload metas:', err);
        payloadMetas = new Map();
        addToast('warning', 'Failed to load payload previews');
      }

      if (gen !== switchGeneration) return;
      loading = false;
    },

    clear(): void {
      thread = null;
      items = [];
      payloadMetas = new Map();
      streamingContent = '';
      activeToolCalls = new Map();
      pendingApprovals = [];
      backgroundTasks = new Map();
      sessionStatus = 'idle';
      tokenUsage = null;
      contextWindow = null;
      rateLimits = [];
      sessionApprovedTools = new Set();
      error = null;
      loading = false;
      pendingMessage = null;
      showTerminal = false;
      showPlanSidebar = false;
      dismissedPlanItemId = null;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      diffPanel.clearForThread();
      turnGeneration++;
      // Invalidate any in-flight switchThread so its late resolutions can't
      // repopulate the pane we just cleared.
      switchGeneration++;
    },

    // --- Mutations (called by event router) ---

    appendTextDelta(delta: string): void {
      streamingContent += delta;
    },

    finalizeTurn(): void {
      streamingContent = '';
      activeToolCalls = new Map();
      pendingMessage = null;
      // Streaming plan updates are a "turn is still thinking" signal —
      // once the turn completes, any finalized plan arrives as an item
      // in the DB reload below. Clearing here prevents a stale in-progress
      // plan from lingering under the next turn's surface.
      pendingPlanUpdate = null;
      // Plan follow-up dismissal belongs to a specific proposed_plan item;
      // once the turn completes, a brand-new proposed_plan may land and the
      // banner should re-appear for that one. Clear the dismissal flag so a
      // previously-dismissed plan doesn't silence the next prompt.
      dismissedPlanItemId = null;
      if (thread) {
        const gen = ++turnGeneration;
        const threadId = thread.id;
        ListItems(threadId).then((loaded) => {
          // Only apply if no newer finalizeTurn or state change has started.
          if (turnGeneration === gen) {
            items = loaded as Item[];
          }
        }).catch((err) => {
          console.error('Failed to reload items after turn:', err);
          addToast('warning', 'Failed to refresh messages');
        });
      }
    },

    addToolCall(id: string, data: unknown): void {
      activeToolCalls = new Map(activeToolCalls).set(id, data);
    },

    updateToolProgress(id: string, progress: ToolProgressMeta): void {
      if (activeToolCalls.has(id)) {
        activeToolCalls = new Map(activeToolCalls).set(id, progress);
      }
    },

    /**
     * Mark a tool call as complete. The actual completed item will arrive
     * from the DB via finalizeTurn -- we just remove the in-progress entry here.
     * No fabricated items are appended to the items array.
     */
    completeToolCall(id: string): void {
      const next = new Map(activeToolCalls);
      next.delete(id);
      activeToolCalls = next;
    },

    setPendingPlanUpdate(meta: unknown): void {
      pendingPlanUpdate = meta;
    },

    clearPendingPlanUpdate(): void {
      pendingPlanUpdate = null;
    },

    addApproval(approval: ApprovalRequest): void {
      pendingApprovals = [...pendingApprovals, approval];
    },

    removeApproval(requestId: string): void {
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    addBackgroundTask(id: string, data: unknown): void {
      backgroundTasks = new Map(backgroundTasks).set(id, data);
    },

    completeBackgroundTask(id: string): void {
      const next = new Map(backgroundTasks);
      next.delete(id);
      backgroundTasks = next;
    },

    setSessionStatus(status: string): void {
      sessionStatus = status;
    },

    setTokenUsage(usage: TokenUsage): void {
      tokenUsage = usage;
    },

    addPayloadMeta(meta: PayloadMeta): void {
      // Bounded LRU insert: keep the most recently used PAYLOAD_META_LIMIT
      // entries so a very long thread can't grow this map without bound.
      // Deleting-then-setting moves the entry to the insertion-order tail
      // (newest), and we evict from the head on overflow.
      const next = new Map(payloadMetas);
      next.delete(meta.id);
      next.set(meta.id, meta);
      while (next.size > PAYLOAD_META_LIMIT) {
        const oldestKey = next.keys().next().value;
        if (oldestKey === undefined) break;
        next.delete(oldestKey);
      }
      payloadMetas = next;
    },

    /**
     * Look up a payload meta and bump it to the LRU tail so heavy reads
     * (scroll through a long thread) keep frequently-used entries alive.
     * Callers that only need a one-shot read can keep using
     * `pane.payloadMetas.get(id)`, but the currently-visible surface should
     * prefer this accessor so live rows survive eviction.
     */
    touchPayloadMeta(id: string): PayloadMeta | undefined {
      const meta = payloadMetas.get(id);
      if (!meta) return undefined;
      const next = new Map(payloadMetas);
      next.delete(id);
      next.set(id, meta);
      payloadMetas = next;
      return meta;
    },

    setError(message: string | null): void {
      error = message;
    },

    clearError(): void {
      error = null;
    },

    setPendingMessage(text: string | null): void {
      pendingMessage = text;
    },

    setContextWindow(data: ContextWindow): void {
      contextWindow = data;
    },

    setRateLimits(limits: RateLimitEntry[]): void {
      rateLimits = limits;
    },

    addSessionApprovedTool(toolName: string): void {
      sessionApprovedTools = new Set(sessionApprovedTools).add(toolName);
    },

    isToolSessionApproved(toolName: string): boolean {
      return sessionApprovedTools.has(toolName);
    },

    updateTitle(title: string): void {
      if (thread) {
        thread = { ...thread, title };
      }
    },

    updateModel(model: string): void {
      if (thread) {
        thread = { ...thread, model };
      }
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

    setDismissedPlanItemId(id: string | null): void {
      dismissedPlanItemId = id;
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
