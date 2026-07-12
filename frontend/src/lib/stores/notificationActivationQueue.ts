// Pure activation-queue factory for OS-notification deep links. It has no
// store imports on purpose: eventsNotification.ts wires the app singleton,
// while unit tests construct queues with stub dependencies and never pull
// the thread/pane stores into the test module graph.
import type { Thread } from '../types/models';
import type { WorkflowItemDetail } from '../types/workflow';

export type NotificationTarget =
  | { kind: 'thread'; threadId: string }
  | { kind: 'workflow-item'; workItemId: string }
  | { kind: 'workflow-triage-agent'; projectId: string }
  | { kind: 'none' };

const pendingActivationCapacity = 8;
const maxNotificationThreadIdBytes = 256;
const maxNotificationWorkItemIdBytes = 256;
const maxNotificationProjectIdBytes = 256;

export interface NotificationActivationDependencies {
  getThreadById(id: string): Thread | undefined | Promise<Thread | undefined>;
  loadThreadById(id: string): Promise<Thread>;
  getWorkflowItem(id: string): Promise<WorkflowItemDetail>;
  createWorkflowTriageAgent(projectId: string): Promise<Thread>;
  openThread(thread: Thread): Promise<unknown>;
  openWorkflowItem(detail: WorkflowItemDetail): void | Promise<unknown>;
  openWorkflowsOverview(): void | Promise<unknown>;
  showError(message: string): void;
  console: Pick<Console, 'info' | 'warn' | 'error'>;
}

export interface NotificationActivationQueue {
  receive(target: unknown): void;
  markHydrated(): Promise<void>;
  pendingCount(): number;
}

function byteLength(value: string): number {
  return new TextEncoder().encode(value).byteLength;
}

export function parseNotificationTarget(value: unknown): NotificationTarget | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const target = value as Record<string, unknown>;
  if (
    (target.threadId !== undefined && typeof target.threadId !== 'string')
    || (target.workItemId !== undefined && typeof target.workItemId !== 'string')
    || (target.projectId !== undefined && typeof target.projectId !== 'string')
  ) return null;
  const threadId = typeof target.threadId === 'string' ? target.threadId : '';
  const workItemId = typeof target.workItemId === 'string' ? target.workItemId : '';
  const projectId = typeof target.projectId === 'string' ? target.projectId : '';
  switch (target.kind) {
    case 'thread':
      return threadId && byteLength(threadId) <= maxNotificationThreadIdBytes && !workItemId && !projectId
        ? { kind: 'thread', threadId }
        : null;
    case 'workflow-item':
      return workItemId && byteLength(workItemId) <= maxNotificationWorkItemIdBytes && !threadId && !projectId
        ? { kind: 'workflow-item', workItemId }
        : null;
    case 'workflow-triage-agent':
      return projectId && byteLength(projectId) <= maxNotificationProjectIdBytes && !threadId && !workItemId
        ? { kind: 'workflow-triage-agent', projectId }
        : null;
    case 'none':
      return !threadId && !workItemId && !projectId ? { kind: 'none' } : null;
    default:
      return null;
  }
}

export function createNotificationActivationQueue(
  dependencies: NotificationActivationDependencies,
): NotificationActivationQueue {
  let hydrated = false;
  let pending: NotificationTarget[] = [];
  let hydrationPromise: Promise<void> | null = null;

  async function apply(target: NotificationTarget): Promise<void> {
    try {
      if (target.kind === 'none') {
        dependencies.console.info('notification:activated: no target');
        return;
      }
      if (target.kind === 'workflow-item') {
        try {
          const detail = await dependencies.getWorkflowItem(target.workItemId);
          await dependencies.openWorkflowItem(detail);
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
          if (message.includes('no rows in result set')) {
            dependencies.showError('This workflow run no longer exists.');
            await dependencies.openWorkflowsOverview();
            return;
          }
          throw error;
        }
        return;
      }
      if (target.kind === 'workflow-triage-agent') {
        const created = await dependencies.createWorkflowTriageAgent(target.projectId);
        const thread = await dependencies.loadThreadById(created.id);
        await dependencies.openThread(thread);
        return;
      }
      const thread = await dependencies.getThreadById(target.threadId);
      if (!thread) {
        dependencies.console.warn(`notification:activated: unknown thread ${target.threadId}`);
        return;
      }
      await dependencies.openThread(thread);
    } catch (error) {
      if (target.kind === 'workflow-item' || target.kind === 'workflow-triage-agent') {
        dependencies.showError(
          target.kind === 'workflow-item'
            ? 'Could not open this workflow run.'
            : 'Could not open the workflow triage agent.',
        );
      }
      dependencies.console.error(
        `notification:activated: failed to open ${target.kind} target`,
        error,
      );
    }
  }

  function receive(target: unknown): void {
    const validated = parseNotificationTarget(target);
    if (!validated) {
      dependencies.console.warn('notification:activated: invalid target', target);
      return;
    }
    if (!hydrated) {
      if (pending.length === pendingActivationCapacity) {
        const dropped = pending.shift();
        dependencies.console.warn('notification:activated: pending queue full; dropped oldest', dropped);
      }
      pending.push(validated);
      return;
    }
    void apply(validated);
  }

  async function markHydrated(): Promise<void> {
    if (hydrated) return;
    if (!hydrationPromise) {
      hydrationPromise = (async () => {
        while (pending.length > 0) {
          const target = pending.shift();
          if (target) await apply(target);
        }
        hydrated = true;
      })();
    }
    await hydrationPromise;
  }

  return {
    receive,
    markHydrated,
    pendingCount: () => pending.length,
  };
}
