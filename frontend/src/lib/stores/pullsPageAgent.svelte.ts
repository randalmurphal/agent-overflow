// "Open with agent" on the page's selected PR (docs/specs/pull-requests.md,
// "The page"): the threads already on it, a new thread in a linked
// worktree on its head, the project's threads, and sending the pinned
// review's drafts to one of them. Every call is pinned to the computer
// the page lists.

import { SvelteMap } from 'svelte/reactivity';
import { GetThread, ListThreadsOnPR, OpenPRThread } from './bindings';
import type { PRThread } from './bindings';
import { mountThreadInPane, syncThreadReply } from './panes.svelte';
import { listedPR, pullsPageBackend, pullsSnapshotSeq, selectedPR, type PullsSelection } from './pullsPage.svelte';
import { currentPinnedReviewState, reviewSubjectForPR } from './reviewPane.svelte';
import { expandProject } from './sidebar.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { getThreads, prependThreadReply } from './threads.svelte';
import { getThreadStatus } from './threadStatuses.svelte';
import { addErrorToast } from './toast.svelte';
import { pullNumberLabel } from '../components/pulls/pullRowView';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { Thread } from '../types/models';
import { errString } from '../utils/errors';
import { relativeTime } from '../utils/format';
import { prReferenceWire } from '../utils/prReference';

/** A thread the menu offers. */
export interface AgentThreadChoice {
  id: string;
  title: string;
  /** "running", "idle · 3d ago": the thread's live status and age. */
  detail: string;
}

interface ThreadsOnPR {
  threads: readonly PRThread[];
  /** The list snapshot the read belongs to; the next one makes it stale. */
  seq: number;
  loading: boolean;
  error: string | null;
}

// By row key. An entry is written wholesale.
const threadsByKey = new SvelteMap<string, ThreadsOnPR>();
const EMPTY: readonly AgentThreadChoice[] = Object.freeze([]);

function choice(id: string, title: string, updatedAt: number, backend: BackendKey): AgentThreadChoice {
  const age = updatedAt > 0 ? relativeTime(updatedAt, 'locale', backend) : '';
  return { id, title: title || 'Untitled thread', detail: [getThreadStatus(id), age].filter(Boolean).join(' · ') };
}

// The PR a call is about: the listed row `key` names (a row's menu), or
// the pane's selection when no key is given (the header's menu).
function targetPR(paneId: string, key?: string): PullsSelection | null {
  return key === undefined ? selectedPR(paneId) : listedPR(paneId, key);
}

/** Reads the threads on the PR (`key`, else the selected one) when the
 * menu opens; a read for the snapshot on screen is kept until the next
 * snapshot. */
export async function loadThreadsOnPR(paneId: string, key?: string): Promise<void> {
  const selection = targetPR(paneId, key);
  if (!selection) return;
  const seq = pullsSnapshotSeq(selection.backend);
  const held = threadsByKey.get(selection.key);
  if (held && held.seq === seq && (held.loading || held.error === null)) return;
  threadsByKey.set(selection.key, { threads: held?.threads ?? [], seq, loading: true, error: null });
  try {
    const threads = await withBackendTarget(selection.backend, () => ListThreadsOnPR({
      projectId: selection.projectId,
      pr: prReferenceWire(selection.pr),
      headRefName: selection.row.headRefName,
      crossRepository: selection.row.crossRepository,
    }));
    if (threadsByKey.get(selection.key)?.seq !== seq) return;
    threadsByKey.set(selection.key, { threads: threads ?? [], seq, loading: false, error: null });
  } catch (err) {
    if (threadsByKey.get(selection.key)?.seq !== seq) return;
    threadsByKey.set(selection.key, { threads: held?.threads ?? [], seq, loading: false, error: errString(err) });
  }
}

function heldThreads(paneId: string, key?: string): { selection: PullsSelection; entry: ThreadsOnPR } | null {
  const selection = targetPR(paneId, key);
  const entry = selection ? threadsByKey.get(selection.key) : undefined;
  return selection && entry ? { selection, entry } : null;
}

/** Reactive: the threads tracking the PR (`key`, else the selected one),
 * as last read. */
export function threadsOnPR(paneId: string, key?: string): readonly AgentThreadChoice[] {
  const held = heldThreads(paneId, key);
  if (!held) return EMPTY;
  return held.entry.threads.map((thread) => choice(thread.threadId, thread.title, thread.updatedAt, held.selection.backend));
}

export function threadsOnPRLoading(paneId: string, key?: string): boolean {
  return heldThreads(paneId, key)?.entry.loading ?? false;
}

export function threadsOnPRError(paneId: string, key?: string): string | null {
  return heldThreads(paneId, key)?.entry.error ?? null;
}

/**
 * The thread whose worktree already has the selected PR's head branch
 * checked out: git refuses a second worktree on it, so "New thread on
 * BRANCH" names this thread instead. A fork's head is the local pr/N
 * branch the backend fetches, which it refuses on its own terms.
 */
export function newThreadCheckedOutBy(paneId: string, key?: string): AgentThreadChoice | null {
  const held = heldThreads(paneId, key);
  if (!held || held.selection.row.crossRepository) return null;
  const branch = held.selection.row.headRefName;
  const thread = held.entry.threads.find((candidate) => candidate.worktreeBranch !== '' && candidate.worktreeBranch === branch);
  return thread ? choice(thread.threadId, thread.title, thread.updatedAt, held.selection.backend) : null;
}

/** Reactive: the project's threads in the sidebar, for sending drafts. */
export function projectThreads(projectId: string, backend: BackendKey): readonly AgentThreadChoice[] {
  return getThreads()
    .filter((thread) => thread.projectId === projectId)
    .map((thread) => choice(thread.id, thread.title, thread.updatedAt, backend));
}

/** The draft a new thread on a PR opens with. */
export function prThreadSeed(selection: PullsSelection): string {
  return `Reviewing ${pullNumberLabel(selection.pr.forge, selection.pr.number)}: ${selection.row.title}\n${selection.row.url}`;
}

/**
 * Opens a thread in a linked worktree on the head of the PR (`key`, else
 * the selected one) with the PR recorded for it, seeds its draft, and
 * mounts it where the page is (the tool pane's slot goes to the thread). A failure after the thread
 * exists removes it and its worktree. Answers whether the thread opened.
 */
export async function openPRThread(paneId: string, key?: string): Promise<boolean> {
  const selection = targetPR(paneId, key);
  if (!selection) return false;
  const { backend, pr, row } = selection;
  const label = pullNumberLabel(pr.forge, pr.number);
  const mark = threadRowFrameMark();
  try {
    // The backend creates the worktree, the thread, the PR record and the
    // seeded draft inside one rollback, so nothing here undoes anything.
    const opened = await withBackendTarget(backend, () => OpenPRThread({
      projectId: selection.projectId,
      pr: prReferenceWire(pr),
      headRefName: row.headRefName,
      crossRepository: row.crossRepository,
      url: row.url,
      nodeId: row.nodeID ?? '',
      title: row.title,
      seed: prThreadSeed(selection),
    }));
    let thread = (await withBackendTarget(backend, () => GetThread(opened.threadId))) as Thread;
    threadsByKey.delete(selection.key);
    thread = prependThreadReply(thread, mark);
    if (thread.projectId) expandProject(thread.projectId);
    await mountThreadInPane(thread);
    return true;
  } catch (err) {
    addErrorToast(`Could not open a thread on ${label}: ${errString(err)}`, err);
    return false;
  }
}

/** Mounts a thread already on the PR where the page is. */
export async function openThread(paneId: string, threadId: string): Promise<boolean> {
  const backend = pullsPageBackend(paneId);
  try {
    const listed = getThreads().find((thread) => thread.id === threadId);
    const thread = listed ?? (backend === null ? null : ((await withBackendTarget(backend, () => GetThread(threadId))) as Thread));
    if (!thread) return false;
    await mountThreadInPane(thread);
    return true;
  } catch (err) {
    addErrorToast(`Could not open the thread: ${errString(err)}`, err);
    return false;
  }
}

/** Sends the selected PR's review drafts to a thread: the pinned review
 * builds their excerpts from the diff it holds, and they are sent on the
 * turn that carries them. */
export async function sendDrafts(paneId: string, threadId: string): Promise<boolean> {
  const selection = selectedPR(paneId);
  if (!selection) return false;
  const state = currentPinnedReviewState(paneId);
  const subject = reviewSubjectForPR({ ownerPaneId: paneId, backend: selection.backend, projectId: selection.projectId, pr: selection.pr });
  if (!state || state.identity !== subject.identity) return false;
  const mark = threadRowFrameMark();
  try {
    const thread = await state.sendDraftsToThread(threadId);
    if (thread) syncThreadReply(thread, mark);
    return thread !== null;
  } catch (err) {
    addErrorToast(`Could not send the review comments: ${errString(err)}`, err);
    return false;
  }
}

export function __resetPullsPageAgentForTest(): void {
  threadsByKey.clear();
}
