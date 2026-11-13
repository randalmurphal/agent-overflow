// The CI actions of one pipeline, per CI key (ciHandles.ts): which apply,
// the one pending, and the artifact downloads running.
//
// Which actions show is the CI read's flags, never a status string: a
// flag of 'allowed' shows the control, and '' (does not apply) or
// 'denied' (this login may not) shows nothing, so no control is ever
// dead. No state is optimistic: an action answers with the pump's
// sequence after the poll it asked for, and the action stays pending
// until the initiating key's applied CI sequence reaches it (the frame
// carrying the forge's answer) or the call fails. A poll that found the
// pipeline unchanged stamped nothing, so an applied sequence already
// there is settled. The sequence is the backend process's: a new launch
// (its hello's launchId) numbers from one again, so the wait ends with
// the launch.
//
// A pending action is held under what the backend reserves for it: the
// workflow runs or the GitLab pipeline it touches, by computer and
// repository (ciRunIdentities). Every CI subject whose pipeline names one
// of them reads it as pending, so a press in a PR's checks is pending on
// the Pipelines tab's view of the same run and the other way round, and
// no second action starts there; the backend refuses it too. It is
// dropped with the initiating subject (dropCIActionsOf) or its CI entry.
// Downloads do not take that slot.
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
  /** The initiating subject's CI key, whose sequence settles it. */
  readonly key: string;
  /** The initiating subject's view (createCIActionsView). */
  readonly owner: symbol;
  /** The run identities the action reserves (ciRunIdentities). */
  readonly runs: readonly string[];
  readonly backend: BackendKey;
  readonly launch: string;
  /** The pump's sequence the answer named; null while the call runs. */
  readonly seq: number | null;
}

const pendingActions = new SvelteMap<number, PendingEntry>();
let nextPending = 0;
// `<key>\n<target>` of each artifact download running (ciArtifactsTarget).
const downloading = new SvelteSet<string>();

/**
 * The run identities of a pipeline, as the backend reserves them for an
 * action (app_ci_actions.go#ciActionKeys): a job action's workflow run or
 * GitLab pipeline, or every run a pipeline action names (its own id and
 * each job's run, which are also every run the pipeline shows). `repo`
 * is the repository's key (repoKey) on `backend`.
 */
export function ciRunIdentities(backend: BackendKey, repo: string, pipeline: CIPipeline | null, job: CIJob | null = null): string[] {
  const ids = new Set<string>();
  if (job?.runId) ids.add(job.runId);
  else if (job && pipeline?.id) ids.add(pipeline.id);
  else {
    if (pipeline?.id) ids.add(pipeline.id);
    for (const stage of pipeline?.stages ?? []) {
      for (const each of stage.jobs) if (each.runId) ids.add(each.runId);
    }
  }
  return [...ids].map((id) => `${backend}\n${repo} ${id}`);
}

function launchOf(backend: BackendKey): string {
  return getTransportHelloFor(backend)?.launchId ?? '';
}

function stillPending(entry: PendingEntry): boolean {
  if (entry.seq === null) return true;
  if (launchOf(entry.backend) !== entry.launch) return false;
  // Between a subscription's runs there is no handle; the next one seeds
  // the sequence it reads.
  const handle = ciHandle(entry.key);
  return handle === null || handle.appliedSeq() < entry.seq;
}

function touches(entry: PendingEntry, key: string, runs: readonly string[]): boolean {
  return entry.key === key || entry.runs.some((run) => runs.includes(run));
}

/** Reactive: the action pending on the pipeline the key shows, whose run
 * identities are `runs`, from whichever subject started it; or null. */
export function peekCIAction(key: string | null, runs: readonly string[] = []): CIPendingAction | null {
  if (key === null) return null;
  for (const entry of pendingActions.values()) {
    if (touches(entry, key, runs) && stillPending(entry)) return entry;
  }
  return null;
}

/** What one press asks for: the subject's key and the runs its pipeline
 * shows (the refusal), and the runs the action reserves (the hold). */
export interface CIActionPress {
  readonly key: string;
  readonly owner: symbol;
  readonly shown: readonly string[];
  readonly reserves: readonly string[];
  readonly backend: BackendKey;
}

/**
 * Runs one action on the key's pipeline. Nothing starts while another is
 * pending there. A failure is a toast with the backend's one sentence
 * and its Details. Answers whether the forge accepted it.
 */
export async function runCIAction(press: CIActionPress, kind: CIActionKind, jobId: string | null): Promise<boolean> {
  const { key, owner, shown, reserves, backend } = press;
  if (untrack(() => peekCIAction(key, shown)) !== null) return false;
  const handle = untrack(() => ciHandle(key));
  if (handle === null) {
    addToast('error', 'The pipeline is still connecting. Try again in a moment.');
    return false;
  }
  untrack(() => {
    for (const [id, entry] of pendingActions) if (!stillPending(entry)) pendingActions.delete(id);
  });
  const call: CIActionCall = jobId === null ? { kind } : { kind, jobId };
  const id = ++nextPending;
  const mine: PendingEntry = { kind, jobId, key, owner, runs: reserves, backend, launch: launchOf(backend), seq: null };
  pendingActions.set(id, mine);
  let seq: number;
  try {
    ({ seq } = await handle.act(call));
  } catch (err) {
    pendingActions.delete(id);
    addErrorToast(errString(err), err);
    return false;
  }
  if (untrack(() => pendingActions.get(id)) !== mine) return true;
  const settled = { ...mine, seq };
  if (untrack(() => stillPending(settled))) pendingActions.set(id, settled);
  else pendingActions.delete(id);
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

/** Drops the pending actions a CI entry started, with the entry. */
export function dropCIActions(key: string): void {
  untrack(() => {
    for (const [id, entry] of pendingActions) if (entry.key === key) pendingActions.delete(id);
  });
}

/** Drops the pending actions a subject's view started, with the view. */
export function dropCIActionsOf(owner: symbol): void {
  untrack(() => {
    for (const [id, entry] of pendingActions) if (entry.owner === owner) pendingActions.delete(id);
  });
}

export function __resetCIActionsForTest(): void {
  pendingActions.clear();
  downloading.clear();
}
