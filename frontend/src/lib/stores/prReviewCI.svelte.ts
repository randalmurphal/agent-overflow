// CI state, keyed by PR.
//
// The head pipeline and the logs of followed jobs belong to the pull
// request, not to the pane showing them: two panes on one PR read one set
// of job rows and one copy of each log. The backend's PR pump owns the CI
// polling, so this state is SOURCED by the PR subscription and nothing here
// polls: the subscribe result seeds the pipeline, `pr:ci_updated` frames
// replace it, and `pr:ci_log` frames move the logs of followed jobs.
// prReviewStore.svelte.ts routes both channels through its wire-key
// aliases and ranks pipeline frames against the PR's one sequence
// watermark; prReviewCIFollows.svelte.ts decides which jobs are followed.
// An entry exists from attachPR until the snapshot store's `onDrop`.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { RefreshPRCI } from './bindings';
import { prSubscriptionId } from './prReviewSubscriptions';
import { withBackendTarget } from '../transport/backends';
import { errString } from '../utils/errors';
import { workspaceKeyBackend } from '../utils/workspaceKey';
import type { CIPipeline } from '../types/models';
import { forgeFailureFrom, type ForgeFailure, type ForgeFailureWire } from '../utils/forgeFailure';

// Wire payload shapes for "pr:ci_updated" and "pr:ci_log". Wails generates
// no TS type for event payloads; kept in sync with PRCIUpdatedEvent and
// PRCILogEvent in internal/app/app_forge_ci.go. A failure's kind fields
// (errorKind, reserve, resumeAt) are ForgeFailureWire's.
export interface PRCIUpdatedEvent extends ForgeFailureWire {
  prKey: string;
  /** Set on a pipeline frame; a PR with no pipeline is an empty one. */
  pipeline?: CIPipeline | null;
  /** Set on an error frame; the pipeline shown before stays. */
  error?: string;
  /** The PR pump's sequence, shared with pr:updated frames. */
  seq?: number;
}

export interface PRCILogEvent extends ForgeFailureWire {
  prKey: string;
  jobId: string;
  seq: number;
  /** The UTF-16 length the receiver must hold for the delta to apply. */
  prevLen: number;
  base: number;
  append: string;
  truncated: boolean;
  totalBytes: number;
  /** False while the forge cannot serve the log: it has not published
   * the log yet (GitHub until the job's log blob exists, running or
   * completed). The text is unchanged. */
  available: boolean;
  error?: string;
}

/** The followed job log as the wire's PRCILogState carries it. */
export interface PRCILogWireState extends ForgeFailureWire {
  text: string;
  truncated: boolean;
  totalBytes: number;
  available: boolean;
  error: string;
  seq: number;
}

/** One followed job's log as this client holds it. Replaced wholesale. */
export interface PRCILogState {
  readonly text: string;
  readonly truncated: boolean;
  readonly totalBytes: number;
  readonly available: boolean;
  readonly error: string | null;
  /** The pump's kind of error; null for a local one or none. */
  readonly failure: ForgeFailure | null;
  /** The backend sequence of the last frame or reply applied. */
  readonly seq: number;
}

export type PRCILogApply = 'applied' | 'ignored' | 'resync';

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

/** Applies one pr:ci_updated frame the PR's watermark admitted. */
export function applyPRCIUpdated(key: string, event: PRCIUpdatedEvent): void {
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

/**
 * Applies one pr:ci_log frame. A frame at or below the job's sequence is
 * already reflected. A frame applies only to text of exactly `prevLen`
 * UTF-16 units; anything else missed a frame and answers 'resync', which
 * asks for the whole text again. Error and unavailable frames are empty
 * deltas against the current length, so they leave the text alone.
 */
export function applyPRCILog(key: string, event: PRCILogEvent): PRCILogApply {
  const entry = entryFor(key);
  if (!entry) return 'ignored';
  const prev = untrack(() => entry.logs.get(event.jobId));
  if (event.seq <= (prev?.seq ?? 0)) return 'ignored';
  const text = prev?.text ?? '';
  if (text.length !== event.prevLen) return 'resync';
  entry.logs.set(event.jobId, {
    text: text.slice(0, event.base) + event.append,
    truncated: event.truncated,
    totalBytes: event.totalBytes,
    available: event.available,
    error: event.error || null,
    failure: event.error ? forgeFailureFrom(event) : null,
    seq: event.seq,
  });
  return 'applied';
}

/**
 * A SetPRCILogFollows reply: the job's full current log. A frame that
 * landed before the reply and moved past it stays.
 */
export function replacePRCILog(key: string, jobId: string, state: PRCILogWireState): void {
  const entry = entryFor(key);
  if (!entry) return;
  const prev = untrack(() => entry.logs.get(jobId));
  if (prev && prev.seq > state.seq) return;
  entry.logs.set(jobId, {
    text: state.text,
    truncated: state.truncated,
    totalBytes: state.totalBytes,
    available: state.available,
    error: state.error || null,
    failure: state.error ? forgeFailureFrom(state) : null,
    seq: state.seq,
  });
}

/** A follow call failed: the job shows the failure over the text it has. */
export function failPRCILog(key: string, jobId: string, message: string): void {
  const entry = entryFor(key);
  if (!entry) return;
  const prev = untrack(() => entry.logs.get(jobId));
  entry.logs.set(jobId, {
    text: prev?.text ?? '',
    truncated: prev?.truncated ?? false,
    totalBytes: prev?.totalBytes ?? 0,
    available: prev?.available ?? true,
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
 * Has the pump poll the pipeline now. The pipeline itself arrives on
 * pr:ci_updated when it changed; the call's failure is shown on the entry
 * until the next refresh or pipeline frame.
 */
export async function refreshPRCI(key: string): Promise<void> {
  const entry = entryFor(key);
  const id = prSubscriptionId(key);
  // No live handle: the subscribe is in flight, and the pump it starts
  // polls CI immediately.
  if (!entry || id === null) return;
  const seq = ++entry.refreshSeq;
  entry.refreshing = true;
  entry.refreshError = null;
  try {
    await withBackendTarget(workspaceKeyBackend(key), () => RefreshPRCI(id));
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
