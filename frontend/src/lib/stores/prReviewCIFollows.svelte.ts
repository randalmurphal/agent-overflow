// Which CI job logs this client follows, per PR, and the SetPRCILogFollows
// calls that tell the backend.
//
// Each pane showing a job log holds one desire per PR key, under its own
// token. The backend keeps the follow set on the subscription handle and
// this client holds one handle per key, so the union of the key's desires
// is what gets sent: when the union changes, after every (re)subscribe (a
// new handle follows nothing), when a pr:ci_log delta does not fit the
// text held, after a transport gap on pr:ci_log, and on a manual refresh
// (re-sending a job id fetches it again). One call per key is in flight
// with at most one owed behind it, so a burst of changes or mismatched
// deltas costs two calls, not one per event. The reply carries each job's
// full current log and replaces the held state unless a frame already
// moved past it (prReviewCI.svelte.ts).

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { SetPRCILogFollows } from './bindings';
import { applyPRCILog, failPRCILog, replacePRCILog, retainPRCILogs, type PRCILogEvent } from './prReviewCI.svelte';
import { prSubscriptionId } from './prReviewSubscriptions';
import { isTransportClassError } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { workspaceKeyBackend } from '../utils/workspaceKey';

class KeyFollows {
  /** Pane token to the job that pane shows. */
  readonly desired = new Map<symbol, string>();
  /** The set the backend handle holds, as far as this client knows. */
  sent: readonly string[] = [];
  /** The jobs the call in flight asked for; null while none is. */
  inFlight: ReadonlySet<string> | null = null;
  owed = false;
  sending = $state(false);

  union(): string[] {
    return [...new Set(this.desired.values())].sort();
  }
}

const followsByKey = new SvelteMap<string, KeyFollows>();

function recordFor(key: string): KeyFollows | undefined {
  return untrack(() => followsByKey.get(key));
}

function settled(record: KeyFollows): boolean {
  return record.desired.size === 0 && record.sent.length === 0 && !record.sending;
}

/**
 * Records the job `token`'s pane shows on the PR, or that it shows none
 * (null). A change to the key's union is sent to the backend.
 */
export function setPRCILogFollow(key: string, token: symbol, jobId: string | null): void {
  let record = recordFor(key);
  if (!record) {
    if (jobId === null) return;
    record = new KeyFollows();
    followsByKey.set(key, record);
  }
  const before = record.union().join('\n');
  if (jobId === null) record.desired.delete(token);
  else record.desired.set(token, jobId);
  const jobs = record.union();
  retainPRCILogs(key, new Set(jobs));
  if (jobs.join('\n') !== before) requestSend(key, record);
  if (settled(record)) followsByKey.delete(key);
}

/** Fetches every followed job of the PR again: the manual log refresh. */
export function refreshPRCILogFollows(key: string): void {
  const record = recordFor(key);
  if (record && record.desired.size > 0) requestSend(key, record);
}

/** A new subscription handle follows nothing: restate the union to it. */
export function resendPRCILogFollows(key: string): void {
  const record = recordFor(key);
  if (!record) return;
  record.sent = [];
  if (record.desired.size > 0) requestSend(key, record);
  else if (settled(record)) followsByKey.delete(key);
}

/** A gap on pr:ci_log: any followed log on the backend may have missed a delta. */
export function resendPRCILogFollowsForBackend(backend: BackendKey): void {
  for (const [key, record] of untrack(() => [...followsByKey])) {
    if (workspaceKeyBackend(key) === backend && record.desired.size > 0) requestSend(key, record);
  }
}

/**
 * Applies a pr:ci_log frame routed to `key`. Frames for jobs another
 * client follows reach this one too and are dropped. A delta that does not
 * fit asks for the full text, unless a call already asking for it is in
 * flight: its reply is at least as new as the frame.
 */
export function applyPRCILogFrame(key: string, event: PRCILogEvent): void {
  const record = recordFor(key);
  if (!record || !record.union().includes(event.jobId)) return;
  if (applyPRCILog(key, event) !== 'resync') return;
  if (record.inFlight?.has(event.jobId)) return;
  requestSend(key, record);
}

/** Reactive: a follow call for the PR is in flight. */
export function prCILogFollowPending(key: string | null): boolean {
  if (key === null) return false;
  return followsByKey.get(key)?.sending ?? false;
}

/** The PR's last holder left; its subscription took the backend's set with it. */
export function dropPRCIFollows(key: string): void {
  const record = recordFor(key);
  if (!record) return;
  record.owed = false;
  followsByKey.delete(key);
}

function requestSend(key: string, record: KeyFollows): void {
  record.owed = true;
  if (record.sending) return;
  void drainFollows(key, record);
}

async function drainFollows(key: string, record: KeyFollows): Promise<void> {
  record.sending = true;
  try {
    while (record.owed && recordFor(key) === record) {
      record.owed = false;
      // No live handle: the subscribe in flight restates the union once it
      // lands (resendPRCILogFollows).
      const id = prSubscriptionId(key);
      if (id === null) return;
      const jobs = record.union();
      if (jobs.length === 0 && record.sent.length === 0) return;
      record.inFlight = new Set(jobs);
      let logs;
      try {
        const result = await withBackendTarget(workspaceKeyBackend(key), () => SetPRCILogFollows(id, jobs));
        logs = result?.logs ?? {};
      } catch (err) {
        // A reply for a handle that is gone is no answer: the resubscribe
        // that replaced it restates the union.
        if (prSubscriptionId(key) !== id || recordFor(key) !== record) continue;
        reportFollowFailure(key, record, jobs, err);
        continue;
      } finally {
        record.inFlight = null;
      }
      if (prSubscriptionId(key) !== id || recordFor(key) !== record) continue;
      record.sent = jobs;
      const followed = new Set(record.union());
      for (const jobId of jobs) {
        const state = logs[jobId];
        if (state && followed.has(jobId)) replacePRCILog(key, jobId, state);
      }
    }
  } finally {
    record.sending = false;
    if (recordFor(key) === record && settled(record)) followsByKey.delete(key);
  }
}

function reportFollowFailure(key: string, record: KeyFollows, jobs: readonly string[], err: unknown): void {
  const followed = new Set(record.union());
  const shown = jobs.filter((jobId) => followed.has(jobId));
  if (shown.length > 0) {
    // A pane is showing these jobs: the failure is its log's state.
    const message = errString(err);
    for (const jobId of shown) failPRCILog(key, jobId, message);
    return;
  }
  // Nobody is showing a log any more and the backend still follows what it
  // was last told. A dead wire needs no clearing: the backend releases the
  // connection's subscriptions when it drops.
  if (isTransportClassError(err)) return;
  reportFrontendDiagnostic('pr ci: clearing log follows failed', errString(err));
}

/** Test seam: forget every follow, as a fresh module load would. */
export function __resetPRCIFollowsForTest(): void {
  for (const record of untrack(() => [...followsByKey.values()])) record.owed = false;
  followsByKey.clear();
}
