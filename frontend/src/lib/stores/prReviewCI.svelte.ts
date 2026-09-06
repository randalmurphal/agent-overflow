import { withBackendTarget } from '../transport/backends';
import { workspaceKeyBackend } from '../utils/workspaceKey';
// CI pipeline state, keyed by PR.
//
// The pipeline belongs to the pull request, not to the pane showing it:
// two panes on one PR read one fetch and one set of job rows. It is not
// SOURCED by the PR subscription though — nothing loads it until a pane
// opens the checks view — so it lives beside the snapshot store rather
// than inside it, and is dropped through that store's `onDrop` when the
// last holder of the PR goes (see prReviewStore.svelte.ts).

import { SvelteMap } from 'svelte/reactivity';
import { GetPRCIJobs } from './bindings';
import { errString } from '../utils/errors';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { CIPipeline } from '../types/models';

class PRCIEntry {
  pipeline = $state<CIPipeline | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  /** Load token: a superseded fetch must not overwrite a newer one. */
  seq = 0;
}

export interface PRCIView {
  readonly pipeline: CIPipeline | null;
  readonly loading: boolean;
  readonly error: string | null;
}

const EMPTY_CI: PRCIView = Object.freeze({ pipeline: null, loading: false, error: null });
const ciByKey = new SvelteMap<string, PRCIEntry>();

function ensureCI(key: string): PRCIEntry {
  let entry = ciByKey.get(key);
  if (!entry) {
    entry = new PRCIEntry();
    ciByKey.set(key, entry);
  }
  return entry;
}

/** Reactive read; a PR nobody has loaded CI for reads as empty, not absent. */
export function peekPRCI(key: string | null): PRCIView {
  if (key === null) return EMPTY_CI;
  return ciByKey.get(key) ?? EMPTY_CI;
}

/**
 * Whether anything has asked for this PR's CI. The poll pump uses it to
 * decide whether a push should refresh the pipeline — a PR whose checks
 * view was never opened must not have work invented for it.
 */
export function hasPRCI(key: string): boolean {
  return ciByKey.has(key);
}

export async function loadPRCIJobs(key: string, ref: PRRef): Promise<void> {
  const entry = ensureCI(key);
  const seq = ++entry.seq;
  entry.loading = true;
  try {
    const pipeline = (await withBackendTarget(workspaceKeyBackend(key), () => GetPRCIJobs(prReferenceWire(ref)))) as CIPipeline;
    if (seq !== entry.seq) return;
    entry.pipeline = pipeline ?? null;
    entry.error = null;
  } catch (err) {
    if (seq !== entry.seq) return;
    entry.error = errString(err);
  } finally {
    if (seq === entry.seq) entry.loading = false;
  }
}

/**
 * Drop a PR's CI state. The token is bumped BEFORE the delete, exactly as
 * dropConflicts does: an in-flight fetch holds the entry object, so
 * without the bump it would write its result into an entry nobody is in
 * the map any more — a resurrection nothing can clear.
 */
export function dropPRCI(key: string): void {
  const entry = ciByKey.get(key);
  if (!entry) return;
  entry.seq += 1;
  ciByKey.delete(key);
}

/** Test seam: drop every entry, as a fresh module load would. */
export function __resetPRCIForTest(): void {
  for (const key of [...ciByKey.keys()]) dropPRCI(key);
}
