import type { Item, PayloadMeta, Thread } from '../types/models';
import type { ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage, ToolProgressMeta } from '../types/events';
import type { ChannelMessage } from '../types/discussion';
import type { DesignArtifact, DesignOptionsRequest, DesignViewport } from '../types/design';
import { ListItems, ListPayloadMetas, SwitchThread } from './bindings';
import { addToast } from './toast.svelte';

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
  let sessionStatus: string = $state('disconnected');
  let tokenUsage: TokenUsage | null = $state(null);
  let contextWindow: ContextWindow | null = $state(null);
  let rateLimits: RateLimitEntry[] = $state([]);
  let sessionApprovedTools: Set<string> = $state(new Set());
  let error: string | null = $state(null);
  let loading: boolean = $state(false);
  let pendingMessage: string | null = $state(null);
  let showTerminal: boolean = $state(false);

  // Channel state (only populated for discussion threads).
  let channelMessages: ChannelMessage[] = $state([]);
  let channelStatus: 'open' | 'concluded' | 'closed' | null = $state(null);

  // Design-mode state (only populated when thread.interactionMode === 'design').
  // designArtifacts is the render+option history for the thread.
  // activeArtifactId is what the preview panel is displaying — null = show latest.
  // pendingDesignOptions is populated when an agent has blocked on present_options.
  // designViewport drives the iframe width toggle.
  let designArtifacts: DesignArtifact[] = $state([]);
  let activeArtifactId: string | null = $state(null);
  let pendingDesignOptions: DesignOptionsRequest | null = $state(null);
  let designViewport: DesignViewport = $state('desktop');

  /**
   * Generation counter for finalizeTurn. Incremented each time finalizeTurn
   * starts so that stale async DB responses don't overwrite newer state.
   */
  let turnGeneration = 0;

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
    get channelMessages() { return channelMessages; },
    get channelStatus() { return channelStatus; },
    get designArtifacts() { return designArtifacts; },
    get activeArtifactId() { return activeArtifactId; },
    get pendingDesignOptions() { return pendingDesignOptions; },
    get designViewport() { return designViewport; },

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
      sessionStatus = 'disconnected';
      error = null;
      pendingMessage = null;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      loading = true;

      thread = newThread;
      // Bump generation so any in-flight finalizeTurn from prior thread is discarded.
      turnGeneration++;

      // Notify the backend so it can auto-start sessions for threads with session_ref.
      try {
        await SwitchThread(newThread.id);
      } catch (err) {
        console.error('Failed to notify backend of thread switch:', err);
        addToast('warning', 'Backend was not notified of thread switch');
      }

      try {
        items = await ListItems(newThread.id) as Item[];
      } catch (err) {
        console.error('Failed to load items:', err);
        items = [];
        error = `Failed to load thread items: ${err}`;
        addToast('error', 'Failed to load thread items');
      }

      try {
        const metas = await ListPayloadMetas(newThread.id) as PayloadMeta[];
        payloadMetas = new Map((metas ?? []).map((m: PayloadMeta) => [m.id, m]));
      } catch (err) {
        console.error('Failed to load payload metas:', err);
        payloadMetas = new Map();
        addToast('warning', 'Failed to load payload previews');
      }

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
      sessionStatus = 'disconnected';
      tokenUsage = null;
      contextWindow = null;
      rateLimits = [];
      sessionApprovedTools = new Set();
      error = null;
      loading = false;
      pendingMessage = null;
      showTerminal = false;
      channelMessages = [];
      channelStatus = null;
      designArtifacts = [];
      activeArtifactId = null;
      pendingDesignOptions = null;
      designViewport = 'desktop';
      turnGeneration++;
    },

    // --- Mutations (called by event router) ---

    appendTextDelta(delta: string): void {
      streamingContent += delta;
    },

    finalizeTurn(): void {
      streamingContent = '';
      activeToolCalls = new Map();
      pendingMessage = null;
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
      payloadMetas = new Map(payloadMetas).set(meta.id, meta);
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
