// A conversation's tracked remote jobs, keyed by thread.
//
// The transcript names a remote job and the computer it ran on the way a
// person knows them: by the job's label and the computer's name. A tool
// row carries only the ids the model used, so it reads the names from
// here. The desktop that ran the job is the authority for both: the label
// was chosen at start, and the computer's name is that desktop's profile
// for it, which a phone reading the transcript has no other way to learn.
//
// Backed by `ListThreadRemoteCommands`, refreshed on the thread's
// `provider:background_tasks_changed`, which the watcher emits on every
// receipt change and the tool server emits when a wait ends. Keyed by the
// thread so every row of one transcript shares one listing, and released
// with the rows: a thread nobody is reading holds no jobs here.

import { ListThreadRemoteCommands, type RemoteJobRecord } from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { wailsEventOn } from './wailsEvents';
import { threadBackend } from '../transport/entityIndex';
import type { BackgroundTasksChangedEvent } from '../types/events';
import { createRefreshScheduler } from '../utils/refreshScheduler';

export type { RemoteJobRecord } from './bindings';

export interface RemoteJobsView {
  /** Every tracked job of the thread by request id. */
  readonly byRequest: ReadonlyMap<string, RemoteJobRecord>;
  /** Computer names the owning desktop knew, by computer id. */
  readonly computerNames: ReadonlyMap<string, string>;
}

// Receipt events come in bursts while a job settles; one listing per burst.
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

function toView(records: readonly RemoteJobRecord[]): RemoteJobsView {
  const byRequest = new Map<string, RemoteJobRecord>();
  const computerNames = new Map<string, string>();
  for (const record of records) {
    byRequest.set(record.requestId, record);
    if (record.computerName && !computerNames.has(record.computerId)) {
      computerNames.set(record.computerId, record.computerName);
    }
  }
  return { byRequest, computerNames };
}

const store = createEntityStore<RemoteJobsView, void>({
  name: 'remoteJobs',
  backendForKey: (threadId) => threadBackend(threadId),
  rawValue: true,
  source: async ({ key, apply, fail, signal }) => {
    const refresh = createRefreshScheduler({
      name: `remoteJobs(${key})`,
      delayMs: REFRESH_DELAY_MS,
      maxWaitMs: REFRESH_MAX_WAIT_MS,
      run: async (token) => {
        try {
          const records = (await ListThreadRemoteCommands(key)) as RemoteJobRecord[] | null;
          if (token.isCurrent()) apply(toView(records ?? []));
        } catch (err) {
          if (token.isCurrent()) fail(err);
        }
      },
    });
    const cancel = wailsEventOn<BackgroundTasksChangedEvent>(
      'provider:background_tasks_changed',
      (evt) => {
        if (evt?.threadId === key) refresh.request();
      },
    );
    let released = false;
    const cleanup = (): void => {
      if (released) return;
      released = true;
      refresh.dispose();
      cancel();
    };
    signal.addEventListener('abort', cleanup);
    refresh.request({ immediate: true });
    return cleanup;
  },
});

/** Hold the thread's job listing for as long as a row needs names from it. */
export function attachRemoteJobs(threadId: string): EntityAttachment<RemoteJobsView> {
  return store.attach(threadId, undefined);
}

/** The tracked job a tool call refers to, or null when unknown here. Reactive. */
export function remoteJobRecord(threadId: string, requestId: string): RemoteJobRecord | null {
  if (!threadId || !requestId) return null;
  return store.peek(threadId)?.byRequest.get(requestId) ?? null;
}

/** The computer's name as the desktop that ran the thread's jobs knew it. Reactive. */
export function remoteJobComputerName(threadId: string, computerId: string): string {
  if (!threadId || !computerId) return '';
  return store.peek(threadId)?.computerNames.get(computerId) ?? '';
}

/**
 * Test seam: suspend() releases every unheld entry; resetAll() lifts the
 * suspension. An entry that survives both was attached and never released.
 */
export function __resetRemoteJobsForTest(): void {
  store.suspend();
  store.resetAll();
}
