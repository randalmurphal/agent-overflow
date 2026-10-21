// The Merge requests tab's projections of the list pump's snapshot: rows
// as PullRowView, the groups after the filters, the project choices and
// failures, and the focus set. Pure, so the page store derives them and
// the tests pin each rule.

import type { PRListItem, PRListSnapshot, PRSeen, ProjectPRList } from './bindings';
import type { MRFilters } from './pullsPageFilters.svelte';
import type { PullRowView } from '../components/pulls/pullRowView';
import type { BackendKey } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { forgeFailureFrom, type ForgeFailure } from '../utils/forgeFailure';
import { relativeTime } from '../utils/format';
import { prKey, prRefFromWire, type PRRef } from '../utils/prReference';
import type { PullGroup, PullSince, PullState } from '../utils/pullsAttention';
import { composeWorkspaceKey } from '../utils/workspaceKey';

/** The most PR keys one SetPRListFocus names (`maxPRListFocus`). */
export const PR_LIST_FOCUS_MAX = 60;

export interface PullsProjectChoice {
  id: string;
  name: string;
}

export interface PullsProjectError {
  projectId: string;
  name: string;
  message: string;
  failure: ForgeFailure | null;
  forge: string;
  /** The failure behind the message, for Details: the pump's correlation
   * id and the forge's raw error as a failed call would carry them; null
   * when the snapshot sent none. */
  cause: TransportError | null;
}

export interface PullsGroup {
  group: PullGroup | string;
  rows: PullRowView[];
}

/** A listed PR with the project it was listed under. */
export interface PullsListedRow {
  key: string;
  pr: PRRef;
  item: PRListItem;
  project: ProjectPRList;
}

// The backend's attention order (prStateOrder in app_pr_attention.go): the
// pump sorts each project's rows by it, and the page merges projects by it.
const STATE_ORDER: readonly PullState[] = ['your_turn', 'pipeline_failed', 'ready', 'changes_requested', 'waiting_on_author', 'draft', 'open'];

function stateRank(state: string): number {
  const at = STATE_ORDER.indexOf(state as PullState);
  return at < 0 ? STATE_ORDER.length : at;
}

/** The computer-qualified PR key the page selects and focuses by. */
export function pullRowKey(backend: BackendKey, pr: PRRef): string {
  return composeWorkspaceKey(backend, prKey(pr));
}

/** Every listed PR once, in project order: a repository two projects
 * clone is listed under the first. */
export function listedRows(snapshot: PRListSnapshot | null, backend: BackendKey): PullsListedRow[] {
  const out: PullsListedRow[] = [];
  const seen = new Set<string>();
  for (const project of snapshot?.projects ?? []) {
    for (const item of project.rows ?? []) {
      const pr = prRefFromWire(item.ref);
      if (!pr) continue;
      const key = pullRowKey(backend, pr);
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({ key, pr, item, project });
    }
  }
  return out;
}

function admits(filters: MRFilters, row: PullsListedRow): boolean {
  if (filters.projects.length > 0 && !filters.projects.includes(row.project.projectId)) return false;
  switch (filters.filter) {
    case 'needs_you':
      return row.item.group === 'needs_you';
    case 'mine':
      return row.item.viewerIsAuthor;
    case 'review_requested':
      return row.item.reviewRequested;
    default:
      return true;
  }
}

function compareRows(a: PRListItem, b: PRListItem): number {
  const ga = a.group === 'needs_you';
  const gb = b.group === 'needs_you';
  if (ga !== gb) return ga ? -1 : 1;
  const byState = stateRank(a.state) - stateRank(b.state);
  if (byState !== 0) return byState;
  const at = Date.parse(a.updatedAt);
  const bt = Date.parse(b.updatedAt);
  if (Number.isFinite(at) && Number.isFinite(bt) && at !== bt) return bt - at;
  return b.ref.Number - a.ref.Number;
}

/** The rows the filters admit, grouped needs-you first, each group in the
 * backend's attention order across projects. A project picked in the
 * filter lists its rows even where another project lists them too. */
export function groupRows(
  snapshot: PRListSnapshot | null,
  backend: BackendKey,
  filters: MRFilters,
  seenOverrides: ReadonlyMap<string, PRSeen>,
): PullsGroup[] {
  const admitted: PullsListedRow[] = [];
  const seen = new Set<string>();
  for (const project of snapshot?.projects ?? []) {
    if (filters.projects.length > 0 && !filters.projects.includes(project.projectId)) continue;
    for (const item of project.rows ?? []) {
      const pr = prRefFromWire(item.ref);
      if (!pr) continue;
      const row = { key: pullRowKey(backend, pr), pr, item, project };
      if (seen.has(row.key) || !admits(filters, row)) continue;
      seen.add(row.key);
      admitted.push(row);
    }
  }
  admitted.sort((a, b) => compareRows(a.item, b.item));
  const groups: PullsGroup[] = [];
  for (const row of admitted) {
    const view = projectPullRow(row, backend, seenOverrides.get(row.key) ?? null);
    const last = groups[groups.length - 1];
    if (last && last.group === row.item.group) last.rows.push(view);
    else groups.push({ group: row.item.group, rows: [view] });
  }
  return groups;
}

// GitHub's head commit rollup in the CI vocabulary the dots read.
function checksStatus(state: string): string {
  switch (state) {
    case 'success':
      return 'success';
    case 'failure':
      return 'failed';
    case 'pending':
      return 'pending';
    default:
      return '';
  }
}

/** Whether a seen record the client holds is newer than the row's. */
function overrides(row: PRListItem, local: PRSeen | null): boolean {
  return local !== null && (row.seen === null || local.openedAt > row.seen.openedAt);
}

export function projectPullRow(row: PullsListedRow, backend: BackendKey, localSeen: PRSeen | null): PullRowView {
  const { item, project } = row;
  const detail = item.detail;
  const local = overrides(item, localSeen) ? localSeen : null;
  return {
    key: row.key,
    forge: row.pr.forge,
    number: row.pr.number,
    title: item.title,
    url: item.url,
    state: item.state,
    group: item.group,
    // Marked viewed just now: what is new is measured from that mark,
    // which records the head the review showed, not always the listed one.
    since: local ? sinceMark(item, local) : sinceOf(item),
    seen: local !== null || item.seen !== null,
    projectName: project.name,
    author: item.viewerIsAuthor ? 'you' : (item.author.name || item.author.login),
    age: ageOf(item.updatedAt, backend),
    draft: item.draft,
    crossRepository: item.crossRepository,
    ci: detail?.pipelineStatus || checksStatus(detail?.checksState || item.checksState),
    approvals: detail && detail.approvalsRequired > 0 ? { given: detail.approvalsGiven, required: detail.approvalsRequired } : null,
    unresolved: detail ? detail.unresolved : null,
  };
}

// The backend's measure (prSinceOf) of the listed PR against a mark;
// null when nothing moved.
function sinceMark(item: PRListItem, seen: PRSeen): PullSince | null {
  const headMoved = item.headSHA !== '' && seen.headSHA !== item.headSHA;
  const newComments = seen.comments === null ? 0 : Math.max(item.comments - seen.comments, 0);
  const newCommits = seen.commits === null ? 0 : Math.max(item.commits - seen.commits, 0);
  return headMoved || newComments > 0 || newCommits > 0 ? { headMoved, newComments, newCommits } : null;
}

function sinceOf(item: PRListItem): PullSince | null {
  const since = item.since;
  return since ? { headMoved: since.headMoved, newComments: since.newComments, newCommits: since.newCommits } : null;
}

function ageOf(updatedAt: string, backend: BackendKey): string {
  const at = Date.parse(updatedAt);
  return Number.isFinite(at) ? relativeTime(at, 'locale', backend) : '';
}

export function projectChoices(snapshot: PRListSnapshot | null): PullsProjectChoice[] {
  return (snapshot?.projects ?? []).map((project) => ({ id: project.projectId, name: project.name }));
}

/** Every failed project the picker admits, with its kind. */
export function projectErrors(snapshot: PRListSnapshot | null, projects: readonly string[], backend: BackendKey): PullsProjectError[] {
  const out: PullsProjectError[] = [];
  for (const project of snapshot?.projects ?? []) {
    if (!project.error) continue;
    if (projects.length > 0 && !projects.includes(project.projectId)) continue;
    out.push({
      projectId: project.projectId,
      name: project.name,
      message: project.error,
      failure: forgeFailureFrom(project),
      forge: project.repo.Forge,
      cause: project.errorDetails
        ? new TransportError(`forge_${project.errorKind || 'forge'}`, project.error, { detail: project.errorDetails, backend })
        : null,
    });
  }
  return out;
}

/**
 * The PR keys (the backend's, `PRReference.Key`) whose detail the page
 * asks for: the rows on screen in list order, at most PR_LIST_FOCUS_MAX.
 * The pump reads the user's own and reviewed rows itself.
 */
export function focusKeys(rows: readonly PullsListedRow[], onScreen: ReadonlySet<string>): string[] {
  const out: string[] = [];
  for (const row of rows) {
    if (out.length >= PR_LIST_FOCUS_MAX) break;
    if (onScreen.has(row.key)) out.push(prKey(row.pr));
  }
  return out;
}
