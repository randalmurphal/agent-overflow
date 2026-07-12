// Pure activation-queue factory for OS-notification deep links. It has no
// store imports on purpose: eventsNotification.ts wires the app singleton,
// while unit tests construct queues with stub dependencies and never pull
// the thread/pane stores into the test module graph.
import type { Thread } from '../types/models';

export interface NotificationTarget {
  kind: 'thread' | 'none';
  threadId?: string;
}

const pendingActivationCapacity = 8;
const maxNotificationThreadIdBytes = 256;

export interface NotificationActivationDependencies {
  getThreadById(id: string): Thread | undefined | Promise<Thread | undefined>;
  openThread(thread: Thread): Promise<unknown>;
  console: Pick<Console, 'info' | 'warn' | 'error'>;
}

export interface NotificationActivationQueue {
  receive(target: NotificationTarget): void;
  markHydrated(): Promise<void>;
  pendingCount(): number;
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
      const thread = await dependencies.getThreadById(target.threadId as string);
      if (!thread) {
        dependencies.console.warn(`notification:activated: unknown thread ${target.threadId}`);
        return;
      }
      await dependencies.openThread(thread);
    } catch (error) {
      dependencies.console.error(
        `notification:activated: failed to open thread ${target.threadId}`,
        error,
      );
    }
  }

  function receive(target: NotificationTarget): void {
    if (!target || (target.kind !== 'thread' && target.kind !== 'none')) {
      dependencies.console.warn('notification:activated: invalid target', target);
      return;
    }
    if (target.kind === 'thread' && !target.threadId) {
      dependencies.console.warn('notification:activated: thread target missing threadId');
      return;
    }
    if (
      target.kind === 'thread'
      && new TextEncoder().encode(target.threadId).byteLength > maxNotificationThreadIdBytes
    ) {
      dependencies.console.warn('notification:activated: threadId is too long');
      return;
    }
    if (target.kind === 'none' && target.threadId) {
      dependencies.console.warn('notification:activated: none target included threadId');
      return;
    }
    if (!hydrated) {
      if (pending.length === pendingActivationCapacity) {
        const dropped = pending.shift();
        dependencies.console.warn('notification:activated: pending queue full; dropped oldest', dropped);
      }
      pending.push(target);
      return;
    }
    void apply(target);
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
