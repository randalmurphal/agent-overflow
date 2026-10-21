// The Pull requests page's Pipelines tab (docs/specs/pull-requests.md,
// "Pipelines tab"): the pipelines of the picked projects on any ref, read
// with one ListProjectPipelines per project when the tab opens, on its
// refresh and every PR update interval while the document is visible;
// stage summaries for the rows on screen (ReadPipelineStages, at most 30
// a call, read again with the list); and the selected pipeline's live CI
// (pipelineCI.svelte.ts) through a CI subject state. Every call is pinned
// to the computer the page lists. A project's failed read fails alone.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ListForgeProjects, ListProjectPipelines, ReadPipelineStages } from './bindings';
import type { ForgeProject, PipelineListResult, PipelineStages } from './bindings';
import { createCISubjectState, ciKeyFor, type CISubjectState } from './ciSubject.svelte';
import { attachPipelineCI, type PipelineCIAttachment } from './pipelineCI.svelte';
import { installPullsReceiver } from './pullsPage.svelte';
import { ensurePullsFilters, pipelineFilters, setPipelineFilters, type PipelineFilters } from './pullsPageFilters.svelte';
import { shortSHA, type PipelineRowView, type PipelineStageView } from '../components/pulls/pipelineRowView';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import { forgeFailureFrom, type ForgeFailure } from '../utils/forgeFailure';
import { relativeTime } from '../utils/format';
import { documentHidden } from '../utils/pageVisibility';
import { repoRefFromWire, repoReferenceWire, type RepoRef } from '../utils/prReference';
import { createRefreshScheduler, type RefreshScheduler } from '../utils/refreshScheduler';

/** The backend's PR update interval (`defaultPRUpdateInterval` in
 * internal/app/app_forge_review.go): the frontend is not told it, so the
 * list re-reads at the default. */
export const PIPELINES_REFRESH_MS = 45_000;
/** The most pipelines one ReadPipelineStages reads (`maxPipelineStageReads`). */
export const PIPELINE_STAGE_READS_MAX = 30;
const STAGES_DELAY_MS = 300;
const STAGES_MAX_WAIT_MS = 1_500;

export interface PipelinesProjectError {
  projectId: string;
  name: string;
  message: string;
  failure: ForgeFailure | null;
  forge: string;
  /** The failed call's error, for Details: its frame carries the log
   * reference and the forge's raw error. */
  cause: unknown;
}

/** One listed pipeline with what reading its stages and CI needs. */
interface ListedPipeline {
  key: string;
  projectId: string;
  repo: RepoRef;
  summary: PipelineListResult['rows'][number];
  projectName: string;
}

export interface PipelinesSelection {
  readonly key: string;
  readonly backend: BackendKey;
  readonly projectId: string;
  readonly repo: RepoRef;
  readonly pipelineId: string;
  /** The row as listed when selected, or as listed now. */
  readonly row: PipelineRowView;
  readonly ci: CISubjectState;
}

function failureOf(err: unknown): ForgeFailure | null {
  // A forge refusal reaches the caller as `forge_<kind>`.
  if (err instanceof TransportError && err.code.startsWith('forge_')) return forgeFailureFrom({ errorKind: err.code.slice('forge_'.length) });
  return null;
}

function stageViews(stages: PipelineStages | undefined): readonly PipelineStageView[] | null {
  if (!stages || stages.error) return null;
  return (stages.stages ?? []).map((stage) => ({ name: stage.name, status: stage.status }));
}

class PipelinesPageBackend {
  readonly panes = new Set<string>();
  projects = $state.raw<readonly ForgeProject[]>([]);
  listed = $state.raw<readonly ListedPipeline[]>([]);
  readonly errors = new SvelteMap<string, PipelinesProjectError>();
  readonly stages = new SvelteMap<string, readonly PipelineStageView[]>();
  loading = $state(false);
  loaded = $state(false);
  refreshing = $state(false);
  /** The projects read failed: nothing else can list. */
  error = $state<string | null>(null);
  readonly list: RefreshScheduler;
  readonly stageReads: RefreshScheduler;
  timer: ReturnType<typeof setTimeout> | null = null;
  lastRead = 0;
  /** The filters the last list read used; another status reads again. */
  readWith: PipelineFilters | null = null;

  readonly rows: readonly PipelineRowView[] = $derived.by(() => {
    const refKind = pipelineFilters(this.backend).refKind;
    return this.listed
      .filter((pipeline) => refKind === '' || pipeline.summary.refKind === refKind)
      .map((pipeline) => this.view(pipeline));
  });
  readonly byKey = $derived.by(() => new Map(this.listed.map((pipeline) => [pipeline.key, pipeline])));

  constructor(readonly backend: BackendKey) {
    this.list = createRefreshScheduler({ name: `pipelines:${backend}`, delayMs: 0, maxWaitMs: 0, run: (token) => this.read(token) });
    this.stageReads = createRefreshScheduler({
      name: `pipelineStages:${backend}`,
      delayMs: STAGES_DELAY_MS,
      maxWaitMs: STAGES_MAX_WAIT_MS,
      run: (token) => this.readStages(token),
    });
  }

  view(pipeline: ListedPipeline): PipelineRowView {
    const { summary } = pipeline;
    const at = Date.parse(summary.createdAt);
    return {
      key: pipeline.key,
      id: summary.id,
      status: summary.status,
      name: summary.name ?? '',
      ref: summary.ref,
      refKind: summary.refKind,
      mrNumber: summary.mrNumber ?? 0,
      forge: pipeline.repo.forge,
      shortSHA: shortSHA(summary.sha),
      title: summary.title ?? '',
      projectName: pipeline.projectName,
      age: Number.isFinite(at) ? relativeTime(at, 'locale', this.backend) : '',
      url: summary.url,
      stages: this.stages.get(pipeline.key) ?? null,
    };
  }

  async read(token: { isCurrent(): boolean }): Promise<void> {
    this.clearTimer();
    this.loading = true;
    const filters = pipelineFilters(this.backend);
    try {
      let projects: ForgeProject[];
      try {
        projects = (await withBackendTarget(this.backend, () => ListForgeProjects())) ?? [];
      } catch (err) {
        if (!token.isCurrent()) return;
        this.error = errString(err);
        return;
      }
      if (!token.isCurrent()) return;
      this.error = null;
      this.projects = projects;
      const picked = projects.filter((project) => !project.error
        && (filters.projects.length === 0 || filters.projects.includes(project.projectId)));
      const reads = await Promise.all(picked.map(async (project) => {
        const repo = repoRefFromWire(project.repo);
        try {
          const result = await withBackendTarget(this.backend, () =>
            ListProjectPipelines(project.projectId, { status: filters.status || undefined }));
          return { project, repo, rows: result?.rows ?? [], error: null };
        } catch (err) {
          return { project, repo, rows: null, error: err };
        }
      }));
      if (!token.isCurrent()) return;
      const listed: ListedPipeline[] = [];
      for (const { project, repo, rows, error } of reads) {
        if (error !== null || rows === null || repo === null) {
          if (error !== null) {
            this.errors.set(project.projectId, {
              projectId: project.projectId, name: project.name, message: errString(error), failure: failureOf(error), forge: project.repo.Forge, cause: error,
            });
          }
          // A failed project keeps the rows it listed before.
          listed.push(...this.listed.filter((pipeline) => pipeline.projectId === project.projectId));
          continue;
        }
        this.errors.delete(project.projectId);
        for (const summary of rows) {
          const key = ciKeyFor({ kind: 'pipeline', backend: this.backend, repo, pipelineId: summary.id });
          listed.push({ key, projectId: project.projectId, repo, summary, projectName: project.name });
        }
      }
      const pickedIds = new Set(picked.map((project) => project.projectId));
      for (const projectId of this.errors.keys()) if (!pickedIds.has(projectId)) this.errors.delete(projectId);
      listed.sort((a, b) => (Date.parse(b.summary.createdAt) || 0) - (Date.parse(a.summary.createdAt) || 0) || Number(b.summary.id) - Number(a.summary.id));
      this.listed = listed;
      this.readWith = filters;
      this.loaded = true;
      // The stages re-read with the list.
      this.stageReads.request({ immediate: true });
    } finally {
      if (token.isCurrent()) {
        this.loading = false;
        this.lastRead = Date.now();
        this.arm();
      }
    }
  }

  async readStages(token: { isCurrent(): boolean }): Promise<void> {
    const onScreen = new Set<string>();
    for (const paneId of this.panes) for (const key of paneEntries.get(paneId)?.onScreen ?? []) onScreen.add(key);
    // On-screen rows by repository, at most one call's worth each.
    const byRepo = new Map<string, { repo: RepoRef; keys: string[]; ids: string[] }>();
    for (const pipeline of this.listed) {
      if (!onScreen.has(pipeline.key)) continue;
      const repoKey = JSON.stringify(repoReferenceWire(pipeline.repo));
      let batch = byRepo.get(repoKey);
      if (!batch) {
        batch = { repo: pipeline.repo, keys: [], ids: [] };
        byRepo.set(repoKey, batch);
      }
      if (batch.ids.length >= PIPELINE_STAGE_READS_MAX) continue;
      batch.keys.push(pipeline.key);
      batch.ids.push(pipeline.summary.id);
    }
    await Promise.all([...byRepo.values()].map(async (batch) => {
      let answer: Record<string, PipelineStages | undefined>;
      try {
        answer = (await withBackendTarget(this.backend, () => ReadPipelineStages(repoReferenceWire(batch.repo), batch.ids))) ?? {};
      } catch {
        // The row shows no dots; the list's own read reports the repository.
        return;
      }
      if (!token.isCurrent()) return;
      batch.ids.forEach((id, index) => {
        const stages = stageViews(answer[id]);
        if (stages) this.stages.set(batch.keys[index]!, stages);
      });
    }));
  }

  /** The next list read, while the document is visible. */
  arm(): void {
    this.clearTimer();
    if (documentHidden() || this.panes.size === 0) return;
    const wait = Math.max(0, this.lastRead + PIPELINES_REFRESH_MS - Date.now());
    this.timer = setTimeout(() => {
      this.timer = null;
      this.list.request({ immediate: true });
    }, wait);
  }

  clearTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  dispose(): void {
    this.clearTimer();
    this.list.dispose();
    this.stageReads.dispose();
  }
}

class PaneEntry {
  selection = $state.raw<{ key: string; projectId: string; repo: RepoRef; pipelineId: string; row: PipelineRowView; ci: CISubjectState; hold: PipelineCIAttachment } | null>(null);
  onScreen: ReadonlySet<string> = new Set();
  constructor(readonly backend: BackendKey) {}
}

const pages = new SvelteMap<BackendKey, PipelinesPageBackend>();
const paneEntries = new SvelteMap<string, PaneEntry>();

/** The pane's Pipelines tab opened: it lists `backend`'s pipelines until
 * detached, and reads them now. */
export function attachPipelinesPage(paneId: string, backend: BackendKey): void {
  untrack(() => {
    const existing = paneEntries.get(paneId);
    if (existing?.backend === backend) return;
    if (existing) detachPipelinesPage(paneId);
    installPullsReceiver();
    ensurePullsFilters(backend);
    let page = pages.get(backend);
    if (!page) {
      page = new PipelinesPageBackend(backend);
      pages.set(backend, page);
    }
    paneEntries.set(paneId, new PaneEntry(backend));
    page.panes.add(paneId);
    page.list.request({ immediate: true });
  });
}

/** The tab or its pane went: the selected pipeline's subscription goes
 * with it; the last pane on a computer stops its reads. */
export function detachPipelinesPage(paneId: string): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    clearPipelineSelection(paneId);
    paneEntries.delete(paneId);
    const page = pages.get(entry.backend);
    if (!page) return;
    page.panes.delete(paneId);
    if (page.panes.size === 0) {
      page.dispose();
      pages.delete(entry.backend);
    }
  });
}

function pageOf(paneId: string): PipelinesPageBackend | null {
  const entry = paneEntries.get(paneId);
  return entry ? (pages.get(entry.backend) ?? null) : null;
}

/** The header's refresh, and what a filter change runs. */
export function refreshPipelinesPage(paneId: string): void {
  pageOf(paneId)?.list.request({ immediate: true });
}

/** Changes the tab's remembered filters: other projects or another
 * status read again; the ref kind filters the rows held. */
export function setPipelinesFilters(paneId: string, next: Partial<PipelineFilters>): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    setPipelineFilters(entry.backend, next);
    const page = pages.get(entry.backend);
    const now = pipelineFilters(entry.backend);
    const was = page?.readWith;
    if (page && (!was || was.status !== now.status || was.projects.join('\n') !== now.projects.join('\n'))) page.list.request({ immediate: true });
  });
}

export function pipelinesRows(paneId: string): readonly PipelineRowView[] {
  return pageOf(paneId)?.rows ?? [];
}

/** Reactive: the first list read has not answered. */
export function pipelinesLoading(paneId: string): boolean {
  const page = pageOf(paneId);
  return page !== null && !page.loaded && page.error === null;
}

/** Reactive: a list read (open, refresh or the interval) is in flight. */
export function pipelinesReading(paneId: string): boolean {
  return pageOf(paneId)?.loading ?? false;
}

/** Reactive: the projects could not be read, so nothing lists. */
export function pipelinesError(paneId: string): string | null {
  return pageOf(paneId)?.error ?? null;
}

export function pipelinesProjects(paneId: string): readonly { id: string; name: string }[] {
  return (pageOf(paneId)?.projects ?? []).filter((project) => !project.error).map((project) => ({ id: project.projectId, name: project.name }));
}

export function pipelinesProjectErrors(paneId: string): readonly PipelinesProjectError[] {
  const page = pageOf(paneId);
  return page ? [...page.errors.values()] : [];
}

/** The rows on screen, by key: their stages are read. */
export function setVisiblePipelines(paneId: string, keys: Iterable<string>): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    if (!entry) return;
    entry.onScreen = new Set(keys);
    pages.get(entry.backend)?.stageReads.request();
  });
}

/** Reactive: the selected pipeline with its live CI. */
export function selectedPipeline(paneId: string): PipelinesSelection | null {
  const entry = paneEntries.get(paneId);
  const selection = entry?.selection ?? null;
  if (!entry || !selection) return null;
  const page = pages.get(entry.backend);
  const listed = page?.byKey.get(selection.key);
  return {
    key: selection.key,
    backend: entry.backend,
    projectId: selection.projectId,
    repo: selection.repo,
    pipelineId: selection.pipelineId,
    row: listed && page ? page.view(listed) : selection.row,
    ci: selection.ci,
  };
}

/** Selects a listed pipeline and follows it; false when none is listed. */
export function selectPipeline(paneId: string, key: string): boolean {
  return untrack(() => {
    const entry = paneEntries.get(paneId);
    const page = entry ? pages.get(entry.backend) : undefined;
    const listed = page?.byKey.get(key);
    if (!entry || !page || !listed) return false;
    if (entry.selection?.key === key) return true;
    clearPipelineSelection(paneId);
    const backend = entry.backend;
    const { repo } = listed;
    const pipelineId = listed.summary.id;
    const hold = attachPipelineCI(backend, repo, pipelineId);
    const ci = createCISubjectState(() => ({ kind: 'pipeline', backend, repo, pipelineId }));
    entry.selection = { key, projectId: listed.projectId, repo, pipelineId, row: page.view(listed), ci, hold };
    return true;
  });
}

export function clearPipelineSelection(paneId: string): void {
  untrack(() => {
    const entry = paneEntries.get(paneId);
    const selection = entry?.selection;
    if (!entry || !selection) return;
    entry.selection = null;
    selection.ci.dispose();
    selection.hold.release();
  });
}

/** The compact probe: a pipeline's detail is open above the list. */
export function pipelineDetailOpen(paneId: string): boolean {
  return (paneEntries.get(paneId)?.selection ?? null) !== null;
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    for (const page of pages.values()) {
      if (documentHidden()) {
        page.clearTimer();
        continue;
      }
      // Back on screen: a read that came due while hidden runs now.
      page.arm();
    }
  });
}

export function __resetPipelinesPageForTest(): void {
  for (const paneId of paneEntries.keys()) detachPipelinesPage(paneId);
}
