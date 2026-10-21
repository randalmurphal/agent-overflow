// The live subscription handle behind each CI key, for the calls the
// backend keeps on a handle: the job log follow set and the manual CI
// refresh. A CI key is the entity key of one pipeline as this client
// holds it: a PR's computer PR key, whose head pipeline its
// `SubscribePRUpdates` handle carries (prReviewStore.svelte.ts), or a
// repository pipeline's `pipeline:<repo key>:<id>` on its computer, which
// a `SubscribePipeline` handle carries (pipelineCI.svelte.ts). The store
// that subscribes registers the handle and clears it in the same cleanup;
// prReviewCI and prReviewCIFollows address it here without knowing which
// kind it is.

import type { PRCILogWireState } from './prReviewCI.svelte';

export interface CIHandle {
  /** Replaces the job logs the handle follows; answers each job's state. */
  follow(jobIds: string[]): Promise<{ logs?: Record<string, PRCILogWireState | undefined> | null } | null | undefined>;
  /** Has the pump read the pipeline now. */
  refresh(): Promise<void>;
}

const handleByKey = new Map<string, CIHandle>();

export function setCIHandle(key: string, handle: CIHandle): void {
  handleByKey.set(key, handle);
}

/** Clears the key's handle only when it is still `handle`: a superseded
 * run's late cleanup must not remove the one the live run installed. */
export function clearCIHandle(key: string, handle: CIHandle): void {
  if (handleByKey.get(key) === handle) handleByKey.delete(key);
}

export function ciHandle(key: string): CIHandle | null {
  return handleByKey.get(key) ?? null;
}

export function __resetCIHandlesForTest(): void {
  handleByKey.clear();
}
