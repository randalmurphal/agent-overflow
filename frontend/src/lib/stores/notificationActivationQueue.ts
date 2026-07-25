// Pure activation-queue factory for OS-notification deep links. It has no
// store imports on purpose: eventsNotification.ts wires the app singleton,
// while unit tests construct queues with stub dependencies and never pull
// the thread/pane stores into the test module graph.
import type { Thread } from '../types/models';

export type NotificationTarget =
  | { kind: 'thread'; threadId: string }
  | { kind: 'workflow-item'; workItemId: string }
  | { kind: 'none' };

const pendingActivationCapacity = 8;
const maxNotificationThreadIdBytes = 256;
const maxNotificationWorkItemIdBytes = 256;

export interface NotificationActivationDependencies {
  getThreadById(id: string): Thread | undefined | Promise<Thread | undefined>;
  openThread(thread: Thread): Promise<unknown>;
  /**
   * Deep link for a parked run (UI-SPEC §7): foreground the app, open the
   * workflows overlay at that run's detail, inside the sweep. The run cache
   * hydrates behind the overlay, so this never needs the run to be loaded
   * first — a target naming a run that no longer exists drops off the stack
   * once the listing lands.
   */
  openWorkflowRun(workItemId: string): Promise<unknown>;
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

// Mirrors internal/notify.ValidateTarget: each kind owns exactly one
// identifier, so a target carrying an identifier from another kind is
// ambiguous and rejected rather than partially honored.
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
  const projectId = Boolean(target.projectId);
  switch (target.kind) {
    case 'thread':
      return threadId && byteLength(threadId) <= maxNotificationThreadIdBytes && !workItemId && !projectId
        ? { kind: 'thread', threadId }
        : null;
    case 'workflow-item':
      return workItemId && byteLength(workItemId) <= maxNotificationWorkItemIdBytes && !threadId && !projectId
        ? { kind: 'workflow-item', workItemId }
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
  let activationChain = Promise.resolve();

  async function apply(target: NotificationTarget): Promise<void> {
    try {
      if (target.kind === 'none') {
        dependencies.console.info('notification:activated: no target');
        return;
      }
      if (target.kind === 'workflow-item') {
        await dependencies.openWorkflowRun(target.workItemId);
        return;
      }
      const thread = await dependencies.getThreadById(target.threadId);
      if (!thread) {
        dependencies.console.warn(`notification:activated: unknown thread ${target.threadId}`);
        return;
      }
      await dependencies.openThread(thread);
    } catch (error) {
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
    activationChain = activationChain.then(() => apply(validated));
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
