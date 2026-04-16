import type { Item, PayloadMeta, Thread } from '../types/models';
import type { ApprovalRequest, ContextWindow, RateLimitEntry, TokenUsage } from '../types/events';
import { ListItems, ListPayloadMetas } from '../../../bindings/agent-overflow/app.js';
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
      loading = true;

      thread = newThread;
      // Bump generation so any in-flight finalizeTurn from prior thread is discarded.
      turnGeneration++;

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

    updateToolProgress(id: string, progress: unknown): void {
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
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
