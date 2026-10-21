// One surface's view of one pipeline: its stages and jobs, and the job log
// the surface opened, with the log's follow, sections, save and copy. The
// review pane drives it for a PR's head pipeline and the Pipelines tab for
// a repository pipeline (docs/specs/pull-requests.md, "Pipelines tab").
//
// A subject names the pipeline. A PR subject reads the CI its PR
// subscription carries (prReviewStore.svelte.ts holds it while the PR is
// attached); a pipeline subject reads what its pipeline subscription
// carries (pipelineCI.svelte.ts, attached by whoever shows it). Either way
// the entry lives under the subject's CI key in prReviewCI.svelte.ts, the
// follow set in prReviewCIFollows.svelte.ts, and a log is read from and
// saved by its repository, whichever PR or pipeline showed it.
//
// The subject is a getter: the review pane's PR moves with its scope. The
// open log records the subject it was opened on, so a read, save or follow
// never addresses a pipeline the surface has since left. A surface that
// holds the subscription itself passes `sourceError`, the reason it cannot
// hold it (its subscribe failing, or a missing grant), which the state
// shows in place of the CI until it clears.

import { SvelteSet } from 'svelte/reactivity';
import { SavePRCIJobLog, SavePRCIJobLogSection } from './bindings';
import { ciLogFetch, readCILogLines as readCILogRange } from './ciLogBlocks.svelte';
import { peekPRCI, refreshPRCI, type PRCILogState } from './prReviewCI.svelte';
import { prCILogFollowPending, refreshPRCILogFollows, setPRCILogFollow } from './prReviewCIFollows.svelte';
import { addToast } from './toast.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { CIJob, CILogLines, CILogSection, CIPipeline } from '../types/models';
import { copyToClipboard } from '../utils/clipboard';
import type { ForgeFailure } from '../utils/forgeFailure';
import { prKey, repoKey, repoReferenceWire, type PRRef, type RepoRef } from '../utils/prReference';
import { composeWorkspaceKey } from '../utils/workspaceKey';

export type CISubject =
  | { kind: 'pr'; backend: BackendKey; pr: PRRef }
  | { kind: 'pipeline'; backend: BackendKey; repo: RepoRef; pipelineId: string };

/** The CI key a subject's state is held under: the computer PR key, or
 * `pipeline:<repo key>:<id>` on the pipeline's computer. */
export function ciKeyFor(subject: CISubject): string {
  if (subject.kind === 'pr') return composeWorkspaceKey(subject.backend, prKey(subject.pr));
  return composeWorkspaceKey(subject.backend, `pipeline:${repoKey(subject.repo)}:${subject.pipelineId}`);
}

function subjectRepo(subject: CISubject): RepoRef {
  if (subject.kind === 'pipeline') return subject.repo;
  const { forge, host, namespace, repo } = subject.pr;
  return { forge, host, namespace, repo };
}

export interface CILogView {
  stageName: string;
  jobId: string;
  job: CIJob;
}

/** A section's text goes into the composer or the clipboard inline up to
 * this size in bytes; a longer one is saved to a file by the backend,
 * from its cached log, and never read into the frontend. */
export const CI_SECTION_INLINE_MAX_BYTES = 64 * 1024;

// SGR and erase sequences (ESC is the one control character that opens
// one): a section's text goes to the clipboard and the chat plain.
const ANSI_SEQUENCE = /\p{Cc}\[[0-9;?]*[A-Za-z]/gu;

/** The log a surface opened, as captured at open. Opaque to callers, who
 * compare it to tell whether the same log is still open after an await. */
export interface CILogOpen {
  readonly key: string;
  readonly subject: CISubject;
  readonly stageName: string;
  readonly job: CIJob & { id: string };
}

export interface CISubjectState {
  /** The subject's CI key; null with no subject. */
  readonly key: string | null;
  readonly pipeline: CIPipeline | null;
  readonly loading: boolean;
  /** A manual CI refresh is in flight. */
  readonly refreshing: boolean;
  readonly error: string | null;
  /** The kind of error; null for a refresh's own failure or none. */
  readonly failure: ForgeFailure | null;
  /** The open job as the pipeline lists it now; null with no log open. */
  readonly logView: CILogView | null;
  readonly log: PRCILogState | null;
  /** A follow call for the open log is in flight. */
  readonly logLoading: boolean;
  readonly logError: string | null;
  /** The kind of logError; null for a local failure or none. */
  readonly logFailure: ForgeFailure | null;
  readonly logSavedPath: string | null;
  /** Expanded sections, `<job id>/<section key>`. */
  readonly logOpenSections: ReadonlySet<string>;
  /** The open log's record; null with none. */
  readonly opened: CILogOpen | null;
  refresh(): Promise<void>;
  openJobLog(stageName: string, job: CIJob): void;
  closeLogView(): void;
  /** Re-sending the follow is the backend's refetch. */
  refreshLog(): void;
  /** Saves the open job's whole log and answers its path, or null with
   * the failure shown. */
  saveLog(): Promise<string | null>;
  toggleSection(sectionKey: string): void;
  setSectionsOpen(sectionKeys: readonly string[], open: boolean): void;
  readLines(from: number, count: number): Promise<CILogLines>;
  /** A section's lines as plain text, read from the backend's cache. */
  sectionText(section: CILogSection): Promise<string>;
  /** Saves a section and answers its path, or null with the failure shown. */
  saveSection(section: CILogSection): Promise<string | null>;
  /** Copies a section: its text inline up to CI_SECTION_INLINE_MAX_BYTES,
   * otherwise the path of the file it is saved to. */
  copySection(section: CILogSection): Promise<boolean>;
  /** Shows a failure of an action on the open log (a send to chat). With
   * `when`, only while that log is still the one open. */
  setLogError(message: string, when?: CILogOpen | null): void;
  /** Releases the open log's follow. */
  dispose(): void;
}

export interface CISubjectOptions {
  /** Reactive: why the surface holds no live subscription; null while it
   * does. Shown over the CI entry, which then reads as not loading. */
  sourceError?: () => string | null;
}

export function createCISubjectState(subject: () => CISubject | null, options: CISubjectOptions = {}): CISubjectState {
  const followToken = Symbol('ci-log');
  const current = $derived(subject());
  const key = $derived(current ? ciKeyFor(current) : null);
  const ci = $derived(peekPRCI(key));
  const sourceError = $derived(options.sourceError?.() ?? null);
  let opened = $state<CILogOpen | null>(null);
  // Save and send failures; the log's own failures are on its state.
  let localError = $state<string | null>(null);
  let savedPath = $state<string | null>(null);
  // Nothing opens on its own, and each log open starts collapsed.
  const openSections = new SvelteSet<string>();

  // The open job as the pipeline lists it now, so status, steps and
  // duration move while the log is watched. A job the pipeline no longer
  // lists (the head moved on) keeps the row captured at open. Both read
  // the pipeline the log was opened on: a PR switch moves the subject
  // before the reload that leaves the old PR closes the view.
  const logView = $derived.by<CILogView | null>(() => {
    const open = opened;
    if (!open) return null;
    const live = findCIJob(peekPRCI(open.key).pipeline, open.job.id);
    return { stageName: live?.stageName ?? open.stageName, jobId: open.job.id, job: live?.job ?? open.job };
  });
  const log = $derived<PRCILogState | null>(opened ? (peekPRCI(opened.key).logs.get(opened.job.id) ?? null) : null);

  function closeLogView(): void {
    if (opened) setPRCILogFollow(opened.key, followToken, null);
    opened = null;
    localError = null;
    savedPath = null;
    openSections.clear();
  }

  function sectionKey(sectionKey: string): string | null {
    return opened ? `${opened.job.id}/${sectionKey}` : null;
  }

  function readLines(from: number, count: number): Promise<CILogLines> {
    const open = opened;
    if (!open) return Promise.reject(new Error('No CI job log is open.'));
    return ciLogFetch(open.subject.backend, repoReferenceWire(subjectRepo(open.subject)), open.job.id)(from, count);
  }

  async function saveSectionOf(open: CILogOpen, section: CILogSection): Promise<string | null> {
    const repo = repoReferenceWire(subjectRepo(open.subject));
    try {
      return String(await withBackendTarget(open.subject.backend, () =>
        SavePRCIJobLogSection(repo, open.job.id, open.job.name, section.key)));
    } catch (err) {
      if (opened === open) localError = userFacingError(err);
      return null;
    }
  }

  async function sectionText(section: CILogSection): Promise<string> {
    const lines = await readCILogRange(readLines, section.start, section.end);
    return lines.join('\n').replace(ANSI_SEQUENCE, '');
  }

  return {
    get key() { return key; },
    get pipeline() { return ci.pipeline; },
    get loading() { return sourceError === null && ci.loading; },
    get refreshing() { return ci.refreshing; },
    get error() { return sourceError ?? ci.error; },
    get failure() { return sourceError === null ? ci.failure : null; },
    get logView() { return logView; },
    get log() { return log; },
    get logLoading() { return opened !== null && prCILogFollowPending(opened.key); },
    get logError() { return localError ?? log?.error ?? null; },
    get logFailure() { return localError === null ? (log?.failure ?? null) : null; },
    get logSavedPath() { return savedPath; },
    get logOpenSections() { return openSections; },
    get opened() { return opened; },

    async refresh() {
      if (key) await refreshPRCI(key);
    },

    openJobLog(stageName, job) {
      const at = current;
      const jobId = job.id;
      if (!at || !key || !job.logsAvailable || !jobId) return;
      if (opened && opened.key !== key) closeLogView();
      opened = { key, subject: at, stageName, job: { ...job, id: jobId } };
      localError = null;
      savedPath = null;
      openSections.clear();
      setPRCILogFollow(key, followToken, jobId);
    },

    closeLogView,

    refreshLog() {
      if (!opened) return;
      localError = null;
      refreshPRCILogFollows(opened.key);
    },

    async saveLog() {
      // The open record, not the derived view: a pipeline frame landing
      // during the save re-derives the view but leaves the same log open.
      const open = opened;
      if (!open) return null;
      const jobName = logView?.job.name ?? open.job.name;
      const repo = repoReferenceWire(subjectRepo(open.subject));
      try {
        const path = String(await withBackendTarget(open.subject.backend, () => SavePRCIJobLog(repo, open.job.id, jobName)));
        if (opened === open) savedPath = path;
        return path;
      } catch (err) {
        if (opened === open) localError = userFacingError(err);
        return null;
      }
    },

    toggleSection(section) {
      const at = sectionKey(section);
      if (at === null) return;
      if (openSections.has(at)) openSections.delete(at);
      else openSections.add(at);
    },

    setSectionsOpen(sections, open) {
      for (const section of sections) {
        const at = sectionKey(section);
        if (at === null) return;
        if (open) openSections.add(at);
        else openSections.delete(at);
      }
    },

    readLines,
    sectionText,

    async saveSection(section) {
      const open = opened;
      return open ? saveSectionOf(open, section) : null;
    },

    async copySection(section) {
      const open = opened;
      if (!open) return false;
      let copied: string;
      if (section.bytes <= CI_SECTION_INLINE_MAX_BYTES) {
        try {
          copied = await sectionText(section);
        } catch (err) {
          if (opened === open) localError = userFacingError(err);
          return false;
        }
      } else {
        const path = await saveSectionOf(open, section);
        if (!path) return false;
        if (opened === open) savedPath = path;
        copied = path;
      }
      if (!(await copyToClipboard(copied))) {
        addToast('error', 'Failed to copy');
        return false;
      }
      if (section.bytes > CI_SECTION_INLINE_MAX_BYTES) {
        addToast('info', 'The section is over 64 KB: it was saved to a file, and the path is copied.');
      }
      return true;
    },

    setLogError(message, when) {
      if (when !== undefined && opened !== when) return;
      localError = message;
    },

    dispose: closeLogView,
  };
}

function findCIJob(pipeline: CIPipeline | null, jobId: string): { stageName: string; job: CIJob } | null {
  for (const stage of pipeline?.stages ?? []) {
    const job = stage.jobs.find((candidate) => candidate.id === jobId);
    if (job) return { stageName: stage.name, job };
  }
  return null;
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'The CI log action failed.';
}
