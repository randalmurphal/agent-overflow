// The CI actions of one pipeline, per CI key (ciHandles.ts): which apply,
// the one pending, and the artifact downloads running. The review pane's
// CI and the Pipelines tab read the same entry through their CI subject
// state (ciSubject.svelte.ts), so a press on one surface is pending on
// every surface showing the pipeline.
//
// Which actions show is the CI read's flags, never a status string: a
// flag of 'allowed' shows the control, and '' (does not apply) or
// 'denied' (this login may not) shows nothing, so no control is ever
// dead. No state is optimistic: an action answers with the pump's
// sequence after the poll it asked for, and the action stays pending
// until the key's applied CI sequence reaches it (the frame carrying the
// forge's answer) or the call fails. A poll that found the pipeline
// unchanged stamped nothing, so an applied sequence already there is
// settled. The sequence is the backend process's: a new launch (its
// hello's launchId) numbers from one again, so the wait ends with the
// launch. While one action is pending on a pipeline no second one starts
// there; the backend refuses it too. Downloads do not take that slot.

import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ciHandle, type CIActionCall } from './ciHandles';
import { addErrorToast, addToast } from './toast.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import type { CIActionFlag, CIJob, CIPipeline } from '../types/models';
import { errString } from '../utils/errors';

export type CIJobActionKind = 'play_job' | 'retry_job' | 'cancel_job';
export type CIPipelineActionKind = 'retry_failed' | 'cancel_pipeline';
export type CIActionKind = CIJobActionKind | CIPipelineActionKind;

export const CI_ACTION_LABEL: Readonly<Record<CIActionKind, string>> = {
  play_job: 'Run job',
  retry_job: 'Retry job',
  cancel_job: 'Cancel job',
  retry_failed: 'Retry failed jobs',
  cancel_pipeline: 'Cancel pipeline',
};

/** What the control reads while its action is pending. */
export const CI_ACTION_PENDING_LABEL: Readonly<Record<CIActionKind, string>> = {
  play_job: 'Starting the job…',
  retry_job: 'Retrying the job…',
  cancel_job: 'Canceling the job…',
  retry_failed: 'Retrying the failed jobs…',
  cancel_pipeline: 'Canceling the pipeline…',
};

function shown(flag: CIActionFlag | undefined): boolean {
  return flag === 'allowed';
}

/** The job actions a job's flags show, in the order the controls sit. */
export function ciJobActionKinds(job: CIJob): CIJobActionKind[] {
  const actions = job.actions;
  if (!job.id || !actions) return [];
  const out: CIJobActionKind[] = [];
  if (shown(actions.play)) out.push('play_job');
  if (shown(actions.retry)) out.push('retry_job');
  if (shown(actions.cancel)) out.push('cancel_job');
  return out;
}

/** The pipeline actions a pipeline's flags show. */
export function ciPipelineActionKinds(pipeline: CIPipeline | null): CIPipelineActionKind[] {
  const actions = pipeline?.actions;
  if (!actions) return [];
  const out: CIPipelineActionKind[] = [];
  if (shown(actions.retryFailed)) out.push('retry_failed');
  if (shown(actions.cancel)) out.push('cancel_pipeline');
  return out;
}

/** A GitLab job's own archive. */
export function ciJobHasArtifacts(job: CIJob): boolean {
  return !!job.id && shown(job.actions?.artifacts);
}

/** A GitHub pipeline's runs' artifacts. */
export function ciPipelineHasArtifacts(pipeline: CIPipeline | null): boolean {
  return shown(pipeline?.actions?.artifacts);
}

/** The action pending on a pipeline. */
export interface CIPendingAction {
  readonly kind: CIActionKind;
  /** The job a job action names; null for a pipeline action. */
  readonly jobId: string | null;
}

interface PendingEntry extends CIPendingAction {
  readonly backend: BackendKey;
  readonly launch: string;
  /** The pump's sequence the answer named; null while the call runs. */
  readonly seq: number | null;
}

const pendingByKey = new SvelteMap<string, PendingEntry>();
// `<key>\n<target>` of each artifact download running (ciArtifactsTarget).
const downloading = new SvelteSet<string>();

function launchOf(backend: BackendKey): string {
  return getTransportHelloFor(backend)?.launchId ?? '';
}

function stillPending(key: string, entry: PendingEntry): boolean {
  if (entry.seq === null) return true;
  if (launchOf(entry.backend) !== entry.launch) return false;
  // Between a subscription's runs there is no handle; the next one seeds
  // the sequence it reads.
  const handle = ciHandle(key);
  return handle === null || handle.appliedSeq() < entry.seq;
}

/** Reactive: the action pending on the key's pipeline, or null. */
export function peekCIAction(key: string | null): CIPendingAction | null {
  if (key === null) return null;
  const entry = pendingByKey.get(key);
  return entry && stillPending(key, entry) ? entry : null;
}

/**
 * Runs one action on the key's pipeline. Nothing starts while another is
 * pending there. A failure is a toast with the backend's one sentence
 * and its Details. Answers whether the forge accepted it.
 */
export async function runCIAction(key: string, backend: BackendKey, kind: CIActionKind, jobId: string | null): Promise<boolean> {
  if (untrack(() => peekCIAction(key)) !== null) return false;
  const handle = untrack(() => ciHandle(key));
  if (handle === null) {
    addToast('error', 'The pipeline is still connecting. Try again in a moment.');
    return false;
  }
  const call: CIActionCall = jobId === null ? { kind } : { kind, jobId };
  const mine: PendingEntry = { kind, jobId, backend, launch: launchOf(backend), seq: null };
  pendingByKey.set(key, mine);
  let seq: number;
  try {
    ({ seq } = await handle.act(call));
  } catch (err) {
    if (untrack(() => pendingByKey.get(key)) === mine) pendingByKey.delete(key);
    addErrorToast(errString(err), err);
    return false;
  }
  if (untrack(() => pendingByKey.get(key)) !== mine) return true;
  const settled = { ...mine, seq };
  if (untrack(() => stillPending(key, settled))) pendingByKey.set(key, settled);
  else pendingByKey.delete(key);
  return true;
}

/** The download key of a job's archive or a pipeline's artifacts. */
export function ciArtifactsTarget(jobId: string | null): string {
  return jobId === null ? 'pipeline' : `job:${jobId}`;
}

/** Reactive: an artifacts download of `target` is running. */
export function ciArtifactsDownloading(key: string | null, target: string): boolean {
  return key !== null && downloading.has(`${key}\n${target}`);
}

/** Runs `save` as the key's `target` download unless one is running;
 * answers its result, or undefined when it did not start. */
export async function trackCIArtifactsDownload<T>(key: string, target: string, save: () => Promise<T>): Promise<T | undefined> {
  const at = `${key}\n${target}`;
  if (untrack(() => downloading.has(at))) return undefined;
  downloading.add(at);
  try {
    return await save();
  } finally {
    downloading.delete(at);
  }
}

/** Drops a pipeline's pending action with its CI entry. */
export function dropCIActions(key: string): void {
  pendingByKey.delete(key);
}

export function __resetCIActionsForTest(): void {
  pendingByKey.clear();
  downloading.clear();
}
