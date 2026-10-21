// CI state, keyed by CI key (ciHandles.ts): a PR's head pipeline under its
// computer PR key, or one repository pipeline the Pipelines tab follows.
//
// The pipeline and the logs of followed jobs belong to that entity, not to
// the pane showing them: two panes on one PR read one set of job rows and
// one copy of each log. The backend's pump owns the CI polling, so this
// state is SOURCED by the subscription and nothing here polls: the
// subscribe result seeds the pipeline, frames replace it, and `pr:ci_log`
// frames move the index of each followed job's log (its lines are read on
// demand, ciLogBlocks.svelte.ts). prReviewStore.svelte.ts feeds a PR's
// (`pr:ci_updated`, ranked against the PR's one sequence watermark) and
// pipelineCI.svelte.ts a repository pipeline's (`ci:pipeline_updated`);
// prReviewCIFollows.svelte.ts decides which jobs are followed. An entry
// exists from the subscribing store's attach until its `onDrop`.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ciHandle } from './ciHandles';
import { errString } from '../utils/errors';
import type { CILogSection, CIPipeline } from '../types/models';
import { forgeFailureFrom, type ForgeFailure, type ForgeFailureWire } from '../utils/forgeFailure';

// Wire payload shapes for "pr:ci_updated" and "pr:ci_log". Wails generates
// no TS type for event payloads; kept in sync with PRCIUpdatedEvent in
// internal/app/app_forge_ci.go and PRCILogEvent in app_forge_ci_log.go. A
// failure's kind fields (errorKind, reserve, resumeAt) are
// ForgeFailureWire's.
export interface PRCIUpdatedEvent extends ForgeFailureWire {
  prKey: string;
  /** Set on a pipeline frame; a PR with no pipeline is an empty one. */
  pipeline?: CIPipeline | null;
  /** Set on an error frame; the pipeline shown before stays. */
  error?: string;
  /** The PR pump's sequence, shared with pr:updated frames. */
  seq?: number;
}

/** A followed job's log as the wire's PRCILogState carries it: the index
 * of the version the backend's cache holds, never its text. */
export interface PRCILogWireState extends ForgeFailureWire {
  /** The version the lines belong to; 0 while the cache holds none. */
  version: number;
  totalLines: number;
  totalBytes: number;
  /** How many leading lines of version - 1 this version keeps. */
  stableLines: number;
  /** The job's rows; a GitHub job's steps are listed before its log. */
  sections: CILogSection[] | null;
  /** False while the forge cannot serve the log: it has not published
   * it yet (GitHub until the job's log blob exists, running or
   * completed). A version held from before stays readable. */
  available: boolean;
  /** A cut download resumes on the backend's next poll. */
  downloading: boolean;
  error: string;
  seq: number;
}

/** The "pr:ci_log" frame: a followed job's whole state whenever it moved.
 * A log is its repository's (repoKey); a frame for a job a pipeline
 * subscription follows carries pipelineKey and an empty prKey. */
export interface PRCILogEvent extends PRCILogWireState {
  prKey: string;
  pipelineKey?: string;
  repoKey?: string;
  jobId: string;
}

/** One followed job's log as this client holds it. Replaced wholesale. */
export interface PRCILogState {
  readonly version: number;
  readonly totalLines: number;
  readonly totalBytes: number;
  readonly stableLines: number;
  readonly sections: readonly CILogSection[];
  readonly available: boolean;
  readonly downloading: boolean;
  readonly error: string | null;
  /** The pump's kind of error; null for a local one or none. */
  readonly failure: ForgeFailure | null;
  /** The backend sequence of the last frame or reply applied. */
  readonly seq: number;
}

class PRCIEntry {
  pipeline = $state<CIPipeline | null>(null);
  /** The pump's CI poll failure, as the subscribe result or a frame said,
   * and its kind. */
  pumpError = $state<string | null>(null);
  pumpFailure = $state<ForgeFailure | null>(null);
  /** The failure of this client's last RefreshPRCI. The pump emits only on
   * change, so it is cleared by the next refresh or pipeline frame. */
  refreshError = $state<string | null>(null);
  refreshing = $state(false);
  /** Refresh token: only the latest refresh settles the shown state. */
  refreshSeq = 0;
  readonly logs = new SvelteMap<string, PRCILogState>();

  /** Subscribed and nothing observed yet: no pipeline and no failure. */
  get loading(): boolean {
    return this.pipeline === null && this.error === null;
  }

  get error(): string | null {
    return this.refreshError ?? this.pumpError;
  }

  /** The kind of the error shown: the pump's, unless a refresh failed. */
  get failure(): ForgeFailure | null {
    return this.refreshError === null ? this.pumpFailure : null;
  }
}

export interface PRCIView {
  readonly pipeline: CIPipeline | null;
  readonly loading: boolean;
  /** A manual RefreshPRCI is in flight. */
  readonly refreshing: boolean;
  readonly error: string | null;
  /** The kind of error; null for a refresh's own failure or none. */
  readonly failure: ForgeFailure | null;
  readonly logs: ReadonlyMap<string, PRCILogState>;
}

const EMPTY_CI: PRCIView = Object.freeze({
  pipeline: null,
  loading: false,
  refreshing: false,
  error: null,
  failure: null,
  logs: new Map<string, PRCILogState>(),
});
const ciByKey = new SvelteMap<string, PRCIEntry>();

function entryFor(key: string): PRCIEntry | undefined {
  return untrack(() => ciByKey.get(key));
}

/** Allocates the key's entry. Called when the PR is attached. */
export function ensurePRCI(key: string): void {
  if (entryFor(key)) return;
  ciByKey.set(key, new PRCIEntry());
}

/** Reactive read; a PR nobody holds reads as empty, not absent. */
export function peekPRCI(key: string | null): PRCIView {
  if (key === null) return EMPTY_CI;
  return ciByKey.get(key) ?? EMPTY_CI;
}

/**
 * The subscribe result's CI. A null pipeline means the pump's first CI poll
 * has not answered; its frame follows on pr:ci_updated. A pipeline held
 * from the key's previous subscription stays until then, as the snapshot
 * does across a re-source.
 */
export function seedPRCI(key: string, pipeline: CIPipeline | null, ciError: string, kind: ForgeFailureWire): void {
  const entry = entryFor(key);
  if (!entry) return;
  if (pipeline) entry.pipeline = pipeline;
  entry.pumpError = ciError || null;
  entry.pumpFailure = ciError ? forgeFailureFrom(kind) : null;
}

/** A pipeline frame as the entry applies it: `pr:ci_updated` for a PR,
 * `ci:pipeline_updated` for a repository pipeline. */
export type CIFrame = Omit<PRCIUpdatedEvent, 'prKey'>;

/** Applies one pipeline frame its key's watermark admitted. */
export function applyPRCIUpdated(key: string, event: CIFrame): void {
  const entry = entryFor(key);
  if (!entry) return;
  if (event.error) {
    entry.pumpError = event.error;
    entry.pumpFailure = forgeFailureFrom(event);
    return;
  }
  if (!event.pipeline) return;
  entry.pipeline = event.pipeline;
  entry.pumpError = null;
  entry.pumpFailure = null;
  entry.refreshError = null;
}

function logState(wire: PRCILogWireState): PRCILogState {
  return {
    version: wire.version,
    totalLines: wire.totalLines,
    totalBytes: wire.totalBytes,
    stableLines: wire.stableLines,
    sections: wire.sections ?? [],
    available: wire.available,
    downloading: wire.downloading,
    error: wire.error || null,
    failure: wire.error ? forgeFailureFrom(wire) : null,
    seq: wire.seq,
  };
}

/**
 * Applies one pr:ci_log frame. A frame carries the job's whole state, so
 * one at or below the job's sequence is already reflected and any other
 * replaces it.
 */
export function applyPRCILog(key: string, event: PRCILogEvent): boolean {
  const entry = entryFor(key);
  if (!entry) return false;
  const prev = untrack(() => entry.logs.get(event.jobId));
  if (event.seq <= (prev?.seq ?? 0)) return false;
  entry.logs.set(event.jobId, logState(event));
  return true;
}

/**
 * A SetPRCILogFollows reply: the job's current state. A frame that
 * landed before the reply and moved past it stays.
 */
export function replacePRCILog(key: string, jobId: string, state: PRCILogWireState): void {
  const entry = entryFor(key);
  if (!entry) return;
  const prev = untrack(() => entry.logs.get(jobId));
  if (prev && prev.seq > state.seq) return;
  entry.logs.set(jobId, logState(state));
}

/** A follow call failed: the job shows the failure over the index it has. */
export function failPRCILog(key: string, jobId: string, message: string): void {
  const entry = entryFor(key);
  if (!entry) return;
  const prev = untrack(() => entry.logs.get(jobId));
  entry.logs.set(jobId, {
    version: prev?.version ?? 0,
    totalLines: prev?.totalLines ?? 0,
    totalBytes: prev?.totalBytes ?? 0,
    stableLines: prev?.stableLines ?? 0,
    sections: prev?.sections ?? [],
    available: prev?.available ?? true,
    downloading: prev?.downloading ?? false,
    error: message,
    failure: null,
    seq: prev?.seq ?? 0,
  });
}

/** Drops the logs of jobs nobody here follows any more. */
export function retainPRCILogs(key: string, jobIds: ReadonlySet<string>): void {
  const entry = entryFor(key);
  if (!entry) return;
  for (const jobId of untrack(() => [...entry.logs.keys()])) {
    if (!jobIds.has(jobId)) entry.logs.delete(jobId);
  }
}

/**
 * Has the pump poll the pipeline now. The pipeline itself arrives on its
 * frame channel when it changed; the call's failure is shown on the entry
 * until the next refresh or pipeline frame.
 */
export async function refreshPRCI(key: string): Promise<void> {
  const entry = entryFor(key);
  const handle = ciHandle(key);
  // No live handle: the subscribe is in flight, and the pump it starts
  // polls CI immediately.
  if (!entry || handle === null) return;
  const seq = ++entry.refreshSeq;
  entry.refreshing = true;
  entry.refreshError = null;
  try {
    await handle.refresh();
  } catch (err) {
    if (seq === entry.refreshSeq) entry.refreshError = errString(err);
  } finally {
    if (seq === entry.refreshSeq) entry.refreshing = false;
  }
}

/** Drop a PR's CI state. A refresh still in flight writes to the dropped
 * entry, which nothing reads. */
export function dropPRCI(key: string): void {
  ciByKey.delete(key);
}

/** Test seam: drop every entry, as a fresh module load would. */
export function __resetPRCIForTest(): void {
  for (const key of untrack(() => [...ciByKey.keys()])) dropPRCI(key);
}
