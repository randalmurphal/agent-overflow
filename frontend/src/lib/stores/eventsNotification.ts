// OS-notification activation routing: `notification:activated` events land
// here and either open the target thread immediately or wait in the bounded
// pre-hydration queue until App.svelte reports the thread registry is
// loaded. The queue mechanics live in notificationActivationQueue.ts; this
// module only binds them to the real thread and pane stores.
import { getThreadById } from './threads.svelte';
import { openThreadInPane } from './panes.svelte';
import {
  createNotificationActivationQueue,
  type NotificationTarget,
} from './notificationActivationQueue';

export type { NotificationTarget } from './notificationActivationQueue';

// The store bindings are referenced inside closures, not at module-eval
// time: suites that partially vi.mock the pane/thread stores (e.g.
// TerminalView.test.ts) import this module transitively via events.ts, and
// vitest mock proxies throw on touching an export the mock factory omitted.
function createAppNotificationActivationQueue() {
  return createNotificationActivationQueue({
    getThreadById: (id) => getThreadById(id),
    openThread: (thread) => openThreadInPane(thread),
    console,
  });
}

let notificationActivationQueue = createAppNotificationActivationQueue();

export function applyNotificationActivated(target: NotificationTarget): void {
  notificationActivationQueue.receive(target);
}

export async function markNotificationHydrated(): Promise<void> {
  await notificationActivationQueue.markHydrated();
}

export function resetNotificationActivationForTest(): void {
  notificationActivationQueue = createAppNotificationActivationQueue();
}
