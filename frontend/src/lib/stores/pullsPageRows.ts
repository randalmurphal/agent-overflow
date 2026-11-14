// The Merge requests tab's projections of the list pump's snapshot: rows
// as PullRowView, the groups after the filters, the find query and the
// sort, the project choices and failures; and the Pipelines tab's find
// match. Pure, so the page stores derive them and the tests pin each rule.
// A find query is a needle from `utils/findMatch.ts`; '' admits every row.

import type { PRListItem, PRListSnapshot, ProjectPRList } from './bindings';
import type { MRFilters, PullsSort, PullsSortKey } from './pullsPageFilters.svelte';
import { pullNumberLabel, pullTimesText, type PullRowView } from '../components/pulls/pullRowView';
import type { PipelineRowView } from '../components/pulls/pipelineRowView';
import type { BackendKey } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { ciStatusText } from '../utils/ciStatus';
import { textsMatchNeedle } from '../utils/findMatch';
import { forgeFailureFrom, type ForgeFailure } from '../utils/forgeFailure';
import { relativeTime } from '../utils/format';
import { prKey, prRefFromWire, type PRRef } from '../utils/prReference';
import { pullStateRank, pullStateText, pullStateTone, type PullGroup } from '../utils/pullsAttention';
import { composeWorkspaceKey } from '../utils/workspaceKey';

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
  /** The rows listed; a detail request failed and their facts are unknown. */
  partial: boolean;
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

/** The computer-qualified PR key the page selects by. */
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

/** The filters a row passes: the toolbar's, and the find needle. */
export type PullsRowFilters = Pick<MRFilters, 'filter' | 'projects'> & { needle: string };

/** A merge request row matches `needle` on what the row shows or names:
 * its title, number (`#123`, GitLab's `!123`, `123`), source branch,
 * author (name, login, or "you"), project and state label. */
export function pullRowMatches(row: PullsListedRow, needle: string): boolean {
  const { item, pr, project } = row;
  return textsMatchNeedle(needle, [
    item.title,
    pullNumberLabel(pr.forge, pr.number),
    `#${pr.number}`,
    item.headRefName,
    item.author.name,
    item.author.login,
    item.viewerIsAuthor ? 'you' : '',
    project.name,
    pullStateText(item.state, item.reason),
  ]);
}

/** A pipeline row matches `needle` on what the row shows: its id (`#9001`),
 * ref (or `!N` for a merge request ref), workflow name, commit title, a
 * prefix of its commit sha, its status in words and its project. */
export function pipelineRowMatches(row: PipelineRowView, needle: string): boolean {
  if (!needle) return true;
  if (row.sha.toLowerCase().startsWith(needle)) return true;
  return textsMatchNeedle(needle, [
    `#${row.id}`,
    row.ref,
    row.refKind === 'merge_request' && row.mrNumber > 0 ? pullNumberLabel(row.forge, row.mrNumber) : '',
    row.name,
    row.title,
    ciStatusText(row.status),
    row.projectName,
  ]);
}

function admits(filters: PullsRowFilters, row: PullsListedRow): boolean {
  if (filters.projects.length > 0 && !filters.projects.includes(row.project.projectId)) return false;
  if (!pullRowMatches(row, filters.needle)) return false;
  switch (filters.filter) {
    case 'needs_you':
      return row.item.group === 'needs_you';
    case 'mine':
      return row.item.viewerIsAuthor;
    case 'reviewing':
      return row.item.viewerIsReviewer;
    default:
      return true;
  }
}

function timeOf(value: string): number {
  const at = Date.parse(value);
  return Number.isFinite(at) ? at : 0;
}

function authorOf(item: PRListItem): string {
  return (item.author.name || item.author.login).toLocaleLowerCase();
}

/**
 * The order within a group under `sort`: a time key newest first, a text
 * key A to Z, `attention` the backend's state order then newest first;
 * `reversed` turns it around. Ties fall to the newest update, then the
 * higher number, so the order is total and stable across snapshots.
 */
export function compareRows(sort: PullsSort, a: PRListItem, b: PRListItem): number {
  let order = 0;
  switch (sort.key) {
    case 'created':
      order = timeOf(b.createdAt) - timeOf(a.createdAt);
      break;
    case 'title':
      order = a.title.localeCompare(b.title, undefined, { sensitivity: 'base' });
      break;
    case 'author':
      order = authorOf(a).localeCompare(authorOf(b), undefined, { sensitivity: 'base' });
      break;
    case 'attention':
      order = pullStateRank(a.state) - pullStateRank(b.state);
      break;
    default:
      order = timeOf(b.updatedAt) - timeOf(a.updatedAt);
  }
  if (order !== 0) return sort.reversed ? -order : order;
  const byUpdate = timeOf(b.updatedAt) - timeOf(a.updatedAt);
  if (byUpdate !== 0) return byUpdate;
  if (a.ref.Number !== b.ref.Number) return b.ref.Number - a.ref.Number;
  // Two repositories' same number: by the repository, so the order never
  // depends on which project lists first.
  return repoOf(a).localeCompare(repoOf(b));
}

function repoOf(item: PRListItem): string {
  return `${item.ref.Host}/${item.ref.Namespace}/${item.ref.Repo}`;
}

/** The rows the filter, project picker and find needle admit, each PR
 * once, unsorted. A project picked in the filter lists its rows even where
 * another project lists them too. */
function admittedRows(snapshot: PRListSnapshot | null, backend: BackendKey, filters: PullsRowFilters): PullsListedRow[] {
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
  return admitted;
}

/** The number of rows the Needs you filter lists for the picked
 * `projects` and the find needle, whichever filter and sort are shown. */
export function needsYouCount(snapshot: PRListSnapshot | null, backend: BackendKey, projects: readonly string[], needle = ''): number {
  return admittedRows(snapshot, backend, { filter: 'needs_you', projects, needle }).length;
}

/** The rows the filters and the find needle admit, grouped needs-you
 * first, each group in the remembered sort across projects. */
export function groupRows(snapshot: PRListSnapshot | null, backend: BackendKey, filters: MRFilters, needle = ''): PullsGroup[] {
  const admitted = admittedRows(snapshot, backend, { filter: filters.filter, projects: filters.projects, needle });
  admitted.sort((a, b) => {
    const ga = a.item.group === 'needs_you';
    const gb = b.item.group === 'needs_you';
    if (ga !== gb) return ga ? -1 : 1;
    return compareRows(filters.sort, a.item, b.item);
  });
  const groups: PullsGroup[] = [];
  for (const row of admitted) {
    const view = projectPullRow(row, backend, filters.sort.key);
    const last = groups[groups.length - 1];
    if (last && last.group === row.item.group) last.rows.push(view);
    else groups.push({ group: row.item.group, rows: [view] });
  }
  return groups;
}

/** `sortKey` picks the time the age shows: the one the list is sorted by. */
export function projectPullRow(row: PullsListedRow, backend: BackendKey, sortKey: PullsSortKey): PullRowView {
  const { item, project } = row;
  return {
    key: row.key,
    forge: row.pr.forge,
    number: row.pr.number,
    title: item.title,
    url: item.url,
    state: item.state,
    group: item.group,
    label: pullStateText(item.state, item.reason),
    tone: pullStateTone(item.state),
    projectName: project.name,
    author: item.viewerIsAuthor ? 'you' : (item.author.name || item.author.login),
    viewerIsAuthor: item.viewerIsAuthor,
    age: ageOf(sortKey === 'created' ? item.createdAt : item.updatedAt, backend),
    times: pullTimesText(item.createdAt, item.updatedAt),
    draft: item.draft,
    crossRepository: item.crossRepository,
    ci: item.ci,
    approvals: item.approvals && item.approvals.required > 0 ? { given: item.approvals.given, required: item.approvals.required } : null,
    unresolved: item.unresolved,
  };
}

// "2h": the relative form without its "ago", which the row's age column
// reads as.
function ageOf(time: string, backend: BackendKey): string {
  const at = Date.parse(time);
  return Number.isFinite(at) ? relativeTime(at, 'locale', backend).replace(/ ago$/, '') : '';
}

export function projectChoices(snapshot: PRListSnapshot | null): PullsProjectChoice[] {
  return (snapshot?.projects ?? []).map((project) => ({ id: project.projectId, name: project.name }));
}

/** Every failed project the picker admits, with its kind: a list that
 * failed, or a detail request that failed beside the rows it lists. */
export function projectErrors(snapshot: PRListSnapshot | null, projects: readonly string[], backend: BackendKey): PullsProjectError[] {
  const out: PullsProjectError[] = [];
  for (const project of snapshot?.projects ?? []) {
    if (projects.length > 0 && !projects.includes(project.projectId)) continue;
    if (project.error) {
      out.push({
        projectId: project.projectId,
        name: project.name,
        message: project.error,
        failure: forgeFailureFrom(project),
        forge: project.repo.Forge,
        partial: false,
        cause: project.errorDetails
          ? new TransportError(`forge_${project.errorKind || 'forge'}`, project.error, { detail: project.errorDetails, backend })
          : null,
      });
    } else if (project.detailError) {
      out.push({ projectId: project.projectId, name: project.name, message: project.detailError, failure: null, forge: project.repo.Forge, partial: true, cause: null });
    }
  }
  return out;
}
