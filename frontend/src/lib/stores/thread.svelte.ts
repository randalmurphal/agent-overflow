import type { Item, PayloadMeta, Thread } from '../types/models';
import type { ApprovalRequest, TokenUsage } from '../types/events';
import { ListItems, ListPayloadMetas } from '../../../wailsjs/go/main/App';

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

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      streamingContent = '';
      activeToolCalls = new Map();
      pendingApprovals = [];
      backgroundTasks = new Map();
      tokenUsage = null;
      sessionStatus = 'disconnected';

      thread = newThread;

      try {
        items = await ListItems(newThread.id);
      } catch (err) {
        console.error('Failed to load items:', err);
        items = [];
      }

      try {
        const metas = await ListPayloadMetas(newThread.id);
        payloadMetas = new Map((metas ?? []).map((m: PayloadMeta) => [m.id, m]));
      } catch (err) {
        console.error('Failed to load payload metas:', err);
        payloadMetas = new Map();
      }
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
    },

    // --- Mutations (called by event router) ---

    appendTextDelta(delta: string): void {
      streamingContent += delta;
    },

    freezeStreamingContent(item: Item): void {
      items = [...items, item];
      streamingContent = '';
    },

    addToolCall(id: string, data: unknown): void {
      activeToolCalls = new Map(activeToolCalls).set(id, data);
    },

    completeToolCall(id: string, item: Item): void {
      const next = new Map(activeToolCalls);
      next.delete(id);
      activeToolCalls = next;
      items = [...items, item];
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

    appendItem(item: Item): void {
      items = [...items, item];
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
