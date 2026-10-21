// A repository pipeline's live CI, for the Pipelines tab: one
// `SubscribePipeline` handle per pipeline this client shows, whatever
// number of surfaces show it (docs/architecture/forge-transport.md, "The
// pipeline pump and stage reads"). It feeds the same CI entry a PR's head
// pipeline has (prReviewCI.svelte.ts), under the pipeline's CI key
// (ciSubject.svelte.ts#ciKeyFor), so a CI subject state reads either kind
// the same way.
//
// The subscribe routes to the selected computer; its handle calls route
// home, so every one is pinned to the computer that answered. Frames on
// `ci:pipeline_updated` carry the pump's sequence: the subscribe result
// and every frame rank against one watermark per key, so an older frame
// never overwrites a newer state. The backend's key for the pipeline is
// aliased to the local key before the subscribe leaves, so a frame the
// pump emits between its join and the answer reaching here routes too: it
// is buffered until the answer seeds the watermark, then replayed in
// sequence order. A pipeline frame and an error-only frame are kept apart,
// so an error newer than the answer does not cost the answer's pipeline.
// A pipeline's followed logs ride `pr:ci_log` with the pipeline's key
// (prReviewCIFollows.svelte.ts).

import {
  RefreshPipeline,
  SetPipelineActive,
  SetPipelineFollows,
  SubscribePipeline,
  UnsubscribePipeline,
} from './bindings';
import { __resetCIHandlesForTest, clearCIHandle, setCIHandle, type CIHandle } from './ciHandles';
import { ciKeyFor } from './ciSubject.svelte';
import { createEntityStore } from './entityStore.svelte';
import { applyPRCIUpdated, dropPRCI, ensurePRCI, seedPRCI } from './prReviewCI.svelte';
import { applyPRCILogFrame, dropPRCIFollows, resendPRCILogFollows } from './prReviewCIFollows.svelte';
import { createPumpVotes } from './pumpVotes';
import { isTransportClassError } from './transportStatus.svelte';
import type { PipelineCILogEvent, PipelineUpdatedEvent } from './pullsStore.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { CIPipeline } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { documentHidden } from '../utils/pageVisibility';
import { repoKey, repoReferenceWire, type RepoRef } from '../utils/prReference';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';

interface PipelineCtx {
  backend: BackendKey;
  repo: RepoRef;
  pipelineId: string;
}

/** A surface's hold on one pipeline's live CI. */
export interface PipelineCIAttachment {
  /** The pipeline's CI key. */
  readonly key: string;
  /** Reactive: the subscribe failed and is retrying; null when healthy. */
  readonly error: string | null;
  release(): void;
}

// The backend's key for a pipeline (`pipelineKey` in
// internal/app/app_pipeline_pump.go), on its computer, to the local keys
// it routes to, each stamped with the source run that installed it: a
// superseded run's late cleanup must not remove the live run's alias.
const localKeysByWireKey = new Map<string, Map<string, symbol>>();
// The highest sequence applied per local key: the subscribe result's or a
// frame's. Cleared when a source run starts, since a restarted backend's
// pump numbers from one again.
const appliedSeqByKey = new Map<string, number>();
// The latest source run per key, settled either way: a gap resync hands
// the runs it started to the recovery fence.
const sourceRunByKey = new Map<string, Promise<void>>();
// The live handle per local key, for the visibility votes.
const subscriptionByKey = new Map<string, { backend: BackendKey; id: string }>();
// The frames routed to a key while its subscribe is in flight, stamped
// with the source run that opened the window: the newest pipeline frame,
// and the newest error-only frame after it (a pipeline frame clears the
// pump's error, so it supersedes an earlier one).
interface JoinFrames {
  readonly owner: symbol;
  pipeline: PipelineUpdatedEvent | null;
  error: PipelineUpdatedEvent | null;
}
const joiningByKey = new Map<string, JoinFrames>();

const votes = createPumpVotes('pipelineCI', (id, active) => SetPipelineActive(id, active));

function wireKeyFor(ctx: PipelineCtx): string {
  return composeWorkspaceKey(ctx.backend, `${repoKey(ctx.repo)}:pipeline:${ctx.pipelineId}`);
}

function addAlias(wireKey: string, key: string, owner: symbol): void {
  let keys = localKeysByWireKey.get(wireKey);
  if (!keys) {
    keys = new Map();
    localKeysByWireKey.set(wireKey, keys);
  }
  keys.set(key, owner);
}

function removeAlias(wireKey: string, key: string, owner: symbol): void {
  const keys = localKeysByWireKey.get(wireKey);
  if (keys?.get(key) !== owner) return;
  keys.delete(key);
  if (keys.size === 0) localKeysByWireKey.delete(wireKey);
}

function admit(key: string, seq: number): boolean {
  const applied = appliedSeqByKey.get(key);
  if (applied !== undefined && seq <= applied) return false;
  appliedSeqByKey.set(key, seq);
  return true;
}

function closeJoin(key: string, owner: symbol): JoinFrames | null {
  const join = joiningByKey.get(key);
  if (join?.owner !== owner) return null;
  joiningByKey.delete(key);
  return join;
}

/** Applies the frames a join buffered, oldest first, through the
 * watermark the subscribe result seeded. */
function replayJoin(key: string, join: JoinFrames | null): void {
  const frames = [join?.pipeline, join?.error].filter((frame): frame is PipelineUpdatedEvent => !!frame);
  frames.sort((a, b) => Number(a.seq ?? 0) - Number(b.seq ?? 0));
  for (const frame of frames) {
    if (admit(key, Number(frame.seq ?? 0))) applyPRCIUpdated(key, frame);
  }
}

type PipelineSourceArgs = Parameters<NonNullable<Parameters<typeof createEntityStore<{ readonly seq: number }, PipelineCtx>>[0]['source']>>[0];

const store = createEntityStore<{ readonly seq: number }, PipelineCtx>({
  name: 'pipelineCI',
  backendForKey: workspaceKeyBackend,
  rawValue: true,
  source: (args) => {
    const run = subscribePipeline(args);
    sourceRunByKey.set(args.key, run.then(() => undefined, () => undefined));
    return run;
  },
  onDrop: (key) => {
    appliedSeqByKey.delete(key);
    joiningByKey.delete(key);
    sourceRunByKey.delete(key);
    dropPRCIFollows(key);
    dropPRCI(key);
  },
});

async function subscribePipeline({ key, getCtx, apply, signal }: PipelineSourceArgs): Promise<() => Promise<void>> {
  {
    const ctx = getCtx();
    const { backend } = ctx;
    const expected = wireKeyFor(ctx);
    const owner = Symbol(key);
    appliedSeqByKey.delete(key);
    joiningByKey.set(key, { owner, pipeline: null, error: null });
    addAlias(expected, key, owner);
    let result;
    try {
      result = await withBackendTarget(backend, () => SubscribePipeline(repoReferenceWire(ctx.repo), ctx.pipelineId));
    } catch (err) {
      closeJoin(key, owner);
      removeAlias(expected, key, owner);
      throw err;
    }
    const id = String(result.id);
    const wireKey = composeWorkspaceKey(backend, String(result.key || ''));
    const handle: CIHandle = {
      follow: (jobIds) => withBackendTarget(backend, () => SetPipelineFollows(id, jobIds)),
      refresh: () => withBackendTarget(backend, () => RefreshPipeline(id)),
    };
    const cleanup = async (): Promise<void> => {
      removeAlias(expected, key, owner);
      removeAlias(wireKey, key, owner);
      clearCIHandle(key, handle);
      if (subscriptionByKey.get(key)?.id === id) subscriptionByKey.delete(key);
      try {
        await withBackendTarget(backend, () => UnsubscribePipeline(id));
      } catch (err) {
        // A dead wire needs no unsubscribe: the backend releases the
        // connection's subscriptions when it drops.
        if (!isTransportClassError(err)) throw err;
      }
    };
    if (signal.aborted) {
      closeJoin(key, owner);
      return cleanup;
    }
    if (wireKey !== expected) {
      // Frames in the join window went to the key this client spelled;
      // the backend's own spelling routes from here on.
      reportFrontendDiagnostic('pipeline ci: the backend named a pipeline by another key', `${expected} != ${wireKey}`);
      addAlias(wireKey, key, owner);
    }
    setCIHandle(key, handle);
    subscriptionByKey.set(key, { backend, id });
    if (documentHidden()) votes.setActive(backend, id, false);
    const seq = Number(result.seq ?? 0);
    // The answer is the key's state as of its sequence; a frame the join
    // buffered and the answer does not account for applies over it.
    appliedSeqByKey.set(key, seq);
    seedPRCI(key, (result.pipeline ?? null) as CIPipeline | null, String(result.error ?? ''), result);
    replayJoin(key, closeJoin(key, owner));
    // The follow set lives on the handle, and this one is new.
    resendPRCILogFollows(key);
    apply({ seq });
    return cleanup;
  }
}

/** Holds a pipeline's live CI until the release. */
export function attachPipelineCI(backend: BackendKey, repo: RepoRef, pipelineId: string): PipelineCIAttachment {
  const key = ciKeyFor({ kind: 'pipeline', backend, repo, pipelineId });
  // Before the attach can source the key: the subscribe result seeds it.
  ensurePRCI(key);
  const handle = store.attach(key, { backend, repo, pipelineId });
  return {
    key,
    get error() {
      return handle.error;
    },
    release: () => handle.release(),
  };
}

/** A `ci:pipeline_updated` frame from `backend`. */
export function applyPipelineFrame(event: PipelineUpdatedEvent, backend: BackendKey): void {
  for (const key of localKeysFor(composeWorkspaceKey(backend, event.key))) {
    const join = joiningByKey.get(key);
    if (join) {
      if (event.error) {
        join.error = event;
      } else if (event.pipeline) {
        join.pipeline = event;
        join.error = null;
      }
      continue;
    }
    if (!admit(key, Number(event.seq ?? 0))) continue;
    applyPRCIUpdated(key, event);
  }
}

/** A `pr:ci_log` frame for a job a pipeline subscription follows. */
export function applyPipelineLogFrame(event: PipelineCILogEvent, backend: BackendKey): void {
  if (!event.jobId) return;
  for (const key of localKeysFor(composeWorkspaceKey(backend, event.pipelineKey))) applyPRCILogFrame(key, event);
}

/** A gap on `ci:pipeline_updated` or a pipeline's `pr:ci_log`: the pump
 * emits only on change, so every pipeline of the computer subscribes
 * again, which restates its state and its follows. Answers the settling
 * of the subscribes it started, or undefined when none did. */
export function resyncPipelinesAfterGap(backend: BackendKey): Promise<void> | undefined {
  const runs: Promise<void>[] = [];
  for (const key of store.keys()) {
    if (workspaceKeyBackend(key) !== backend) continue;
    const before = sourceRunByKey.get(key);
    store.invalidate(key);
    const run = sourceRunByKey.get(key);
    if (run && run !== before) runs.push(run);
  }
  return runs.length === 0 ? undefined : Promise.all(runs).then(() => undefined);
}

function localKeysFor(wireKey: string): string[] {
  const keys = localKeysByWireKey.get(wireKey);
  return keys ? [...keys.keys()] : [];
}

/** Each pipeline handle reports this window's visibility; the backend
 * composes them, so one visible client keeps a shared pump running. */
export function handlePipelineVisibilityChange(): void {
  const active = !documentHidden();
  for (const { backend, id } of subscriptionByKey.values()) votes.setActive(backend, id, active);
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', handlePipelineVisibilityChange);
}

/** Tests: the pipelines held. */
export function pipelineCIKeys(): string[] {
  return store.keys();
}

export function __resetPipelineCIForTest(): void {
  store.suspend();
  store.resetAll();
  localKeysByWireKey.clear();
  appliedSeqByKey.clear();
  sourceRunByKey.clear();
  joiningByKey.clear();
  subscriptionByKey.clear();
  votes.resetForTest();
  __resetCIHandlesForTest();
}
