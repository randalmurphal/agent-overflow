// The Pull requests page's remembered filters, per computer and per tab
// (docs/specs/pull-requests.md, "The page"): the Merge requests tab's
// filter, project picker and sort, and the Pipelines tab's projects, ref
// kind and status. Persisted in appStorage under the computer the page
// lists; adopted again once hydration lands (App.svelte boot), as the
// other remembered view preferences are.

import { appStorageGet, appStorageSet } from './appStorage';
import type { BackendKey } from '../transport/backendKey';

export type PullsFilter = 'needs_you' | 'mine' | 'reviewing' | 'all';

/** What the Merge requests tab sorts by, within each group. */
export type PullsSortKey = 'updated' | 'created' | 'title' | 'author' | 'attention';

export interface PullsSort {
  readonly key: PullsSortKey;
  /** Reversed from the key's natural order (newest first, A to Z, the
   * attention order). */
  readonly reversed: boolean;
}

export interface MRFilters {
  readonly filter: PullsFilter;
  /** Project ids; empty means every project. */
  readonly projects: readonly string[];
  readonly sort: PullsSort;
}

export type PipelineRefKind = '' | 'branch' | 'tag' | 'merge_request';
/** A normalized status the list filters by (PipelineListFilter.Status);
 * '' is every status. */
export type PipelineStatusFilter = '' | 'success' | 'failed' | 'running' | 'pending' | 'canceled' | 'skipped' | 'manual';

export interface PipelineFilters {
  readonly projects: readonly string[];
  readonly refKind: PipelineRefKind;
  readonly status: PipelineStatusFilter;
}

export const MR_FILTERS_KEY = 'pulls:filter:mrs';
export const PIPELINE_FILTERS_KEY = 'pulls:filter:pipelines';

const FILTERS = new Set<string>(['needs_you', 'mine', 'reviewing', 'all']);
const SORT_KEYS = new Set<string>(['updated', 'created', 'title', 'author', 'attention']);
const REF_KINDS = new Set<string>(['', 'branch', 'tag', 'merge_request']);
const STATUSES = new Set<string>(['', 'success', 'failed', 'running', 'pending', 'canceled', 'skipped', 'manual']);

export const DEFAULT_MR_SORT: PullsSort = Object.freeze({ key: 'updated', reversed: false });
export const DEFAULT_MR_FILTERS: MRFilters = Object.freeze({ filter: 'needs_you', projects: Object.freeze([]), sort: DEFAULT_MR_SORT });
export const DEFAULT_PIPELINE_FILTERS: PipelineFilters = Object.freeze({ projects: Object.freeze([]), refKind: '', status: '' });

function parseObject(raw: string | null): Record<string, unknown> | null {
  if (raw === null) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

function parseProjects(value: unknown): string[] {
  return Array.isArray(value) ? [...new Set(value.filter((id): id is string => typeof id === 'string' && id !== ''))] : [];
}

function parseSort(value: unknown): PullsSort {
  if (!value || typeof value !== 'object') return DEFAULT_MR_SORT;
  const sort = value as Record<string, unknown>;
  const key = typeof sort.key === 'string' && SORT_KEYS.has(sort.key) ? (sort.key as PullsSortKey) : DEFAULT_MR_SORT.key;
  return { key, reversed: sort.reversed === true };
}

export function parseMRFilters(raw: string | null): MRFilters {
  const parsed = parseObject(raw);
  if (!parsed) return DEFAULT_MR_FILTERS;
  // `review_requested` was the filter's name before it listed every MR
  // the viewer reviews.
  const stored = parsed.filter === 'review_requested' ? 'reviewing' : parsed.filter;
  const filter = typeof stored === 'string' && FILTERS.has(stored) ? (stored as PullsFilter) : DEFAULT_MR_FILTERS.filter;
  return { filter, projects: parseProjects(parsed.projects), sort: parseSort(parsed.sort) };
}

export function parsePipelineFilters(raw: string | null): PipelineFilters {
  const parsed = parseObject(raw);
  if (!parsed) return DEFAULT_PIPELINE_FILTERS;
  const refKind = typeof parsed.refKind === 'string' && REF_KINDS.has(parsed.refKind) ? (parsed.refKind as PipelineRefKind) : '';
  const status = typeof parsed.status === 'string' && STATUSES.has(parsed.status) ? (parsed.status as PipelineStatusFilter) : '';
  return { projects: parseProjects(parsed.projects), refKind, status };
}

class BackendFilters {
  mrs = $state.raw<MRFilters>(DEFAULT_MR_FILTERS);
  pipelines = $state.raw<PipelineFilters>(DEFAULT_PIPELINE_FILTERS);

  constructor(readonly backend: BackendKey) {
    this.sync();
  }

  sync(): void {
    this.mrs = parseMRFilters(appStorageGet(MR_FILTERS_KEY, this.backend));
    this.pipelines = parsePipelineFilters(appStorageGet(PIPELINE_FILTERS_KEY, this.backend));
  }
}

const filtersByBackend = new Map<BackendKey, BackendFilters>();

/** Allocates a computer's filters from storage. Called where a page
 * attaches, never from a getter a `$derived` reads. */
export function ensurePullsFilters(backend: BackendKey): void {
  if (!filtersByBackend.has(backend)) filtersByBackend.set(backend, new BackendFilters(backend));
}

/** Reactive; the defaults before the computer's page attached. */
export function mrFilters(backend: BackendKey): MRFilters {
  return filtersByBackend.get(backend)?.mrs ?? DEFAULT_MR_FILTERS;
}

export function pipelineFilters(backend: BackendKey): PipelineFilters {
  return filtersByBackend.get(backend)?.pipelines ?? DEFAULT_PIPELINE_FILTERS;
}

export function setMRFilters(backend: BackendKey, next: Partial<MRFilters>): void {
  ensurePullsFilters(backend);
  const entry = filtersByBackend.get(backend)!;
  const merged: MRFilters = { ...entry.mrs, ...next, projects: [...(next.projects ?? entry.mrs.projects)] };
  entry.mrs = merged;
  appStorageSet(MR_FILTERS_KEY, JSON.stringify(merged), backend);
}

export function setPipelineFilters(backend: BackendKey, next: Partial<PipelineFilters>): void {
  ensurePullsFilters(backend);
  const entry = filtersByBackend.get(backend)!;
  const merged: PipelineFilters = { ...entry.pipelines, ...next, projects: [...(next.projects ?? entry.pipelines.projects)] };
  entry.pipelines = merged;
  appStorageSet(PIPELINE_FILTERS_KEY, JSON.stringify(merged), backend);
}

/** Adopt the durable copies once appStorage hydration lands. */
export function syncPullsFiltersFromAppStorage(): void {
  for (const entry of filtersByBackend.values()) entry.sync();
}

export function resetPullsFiltersForTest(): void {
  filtersByBackend.clear();
}
