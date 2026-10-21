// The Pull requests page's Pipelines tab (docs/specs/pull-requests.md,
// "Pipelines tab"): the pipelines of the picked projects on any ref, read
// with one ListProjectPipelines per repository (through the first picked
// project that clones it, which lists its rows) when the tab opens, on its
// refresh and every PR update interval while the document is visible;
// stage summaries for the rows on screen (ReadPipelineStages, at most 30
// a call, read again with the list, held only for listed rows); and the
// selected pipeline's live CI (pipelineCI.svelte.ts) through a CI subject
// state. Every call is pinned to the computer the page lists. A project's
// failed read, or a project whose origin could not be read, fails alone;
// a failed stage read shows on its rows in place of their dots.
//
// Every call is `git:operate`. The passive reads (open, interval, filters,
// stages, the selected pipeline's subscription) run only while the
// computer is connected and that grant is known and held; they stop on a
// disconnect, a revoked grant or the computer's removal, discarding the
// reads in flight, and the list reads again at once when both hold again.
// While the document is hidden the list and stage reads wait: one that
// comes due then runs when it shows. The refresh button reads regardless,
// so a refusal shows.
// Opening the tab, its refresh and a filter change are the user's reads,
// which may spend the forge's rate limit reserve; the interval and
// scrolling read in the background, which may not.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ListForgeProjects, ListProjectPipelines, ReadPipelineStages } from './bindings';
import type { ForgeProject, PipelineListResult, PipelineStages } from './bindings';
import { createCISubjectState, ciKeyFor, type CISubjectState } from './ciSubject.svelte';
import { attachPipelineCI, type PipelineCIAttachment } from './pipelineCI.svelte';
import { installPullsReceiver } from './pullsPage.svelte';
import { ensurePullsFilters, pipelineFilters, setPipelineFilters, type PipelineFilters } from './pullsPageFilters.svelte';
import { getTransportStatusFor } from './transportStatus.svelte';
import { shortSHA, type PipelineRowView, type PipelineStageView } from '../components/pulls/pipelineRowView';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { presentScope, scopeRefusalMessage } from '../transport/scopeRefusal';
import { hasScope, pageGrantsKnown, pageGrantsKnownTracked } from '../transport/scopes';
import { TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import { forgeFailureFrom, type ForgeFailure } from '../utils/forgeFailure';
import { relativeTime } from '../utils/format';
import { documentHidden } from '../utils/pageVisibility';
import { repoKey, repoRefFromWire, repoReferenceWire, type RepoRef } from '../utils/prReference';
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
  /** `repoKey(repo)`: a repository's pipelines are read once. */
  repoId: string;
  summary: PipelineListResult['rows'][number];
  projectName: string;
}

/** A listed row's last stage read: its stages, or why they could not be read. */
interface StageRead {
  readonly stages: readonly PipelineStageView[] | null;
  readonly error: string | null;
}

/** Whether the computer granted this page `git:operate`; unknown until
 * the page's grants resolve. */
type Grant = 'unknown' | 'granted' | 'refused';

function notGrantedMessage(): string {
  const { title, hint } = presentScope('git:operate');
  return `${title} ${hint}`;
}

function readError(err: unknown): string {
  return scopeRefusalMessage(err) ?? errString(err);
}

function grantOf(backend: BackendKey, known: boolean): Grant {
  if (!known) return 'unknown';
  return hasScope('git:operate', backend) ? 'granted' : 'refused';
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
  if (err instanceof TransportError && err.code.startsWith('forge_')) {
    return forgeFailureFrom({ errorKind: err.code.slice('forge_'.length), reserve: err.reserve, resumeAt: err.resumeAt });
  }
  return null;
}

function stageRead(stages: PipelineStages): StageRead {
  if (stages.error) return { stages: null, error: stages.error };
  return { stages: (stages.stages ?? []).map((stage) => ({ name: stage.name, status: stage.status })), error: null };
}

class PipelinesPageBackend {
  readonly panes = new Set<string>();
  projects = $state.raw<readonly ForgeProject[]>([]);
  listed = $state.raw<readonly ListedPipeline[]>([]);
  readonly errors = new SvelteMap<string, PipelinesProjectError>();
  /** Stage reads of listed rows only, by row key. */
  readonly stages = new SvelteMap<string, StageRead>();
  loading = $state(false);
  loaded = $state(false);
  /** The projects read failed: nothing else can list. */
  error = $state<string | null>(null);
  grant = $state<Grant>('unknown');
  /** The owning computer's connection, as its status box last said. */
  connected = false;
  /** The next list read is the refresh button's, which runs ungated. */
  explicit = false;
  /** A list or stage read came due while the document was hidden; it
   * runs when the document shows. */
  listDeferred = false;
  stagesDeferred = false;
  readonly list: RefreshScheduler;
  readonly stageReads: RefreshScheduler;
  timer: ReturnType<typeof setTimeout> | null = null;
  lastRead = 0;
  /** The filters the last list read used; another status reads again. */
  readWith: PipelineFilters | null = null;
  readonly stopWatching: () => void;
  /** A pending list read is the user's action (open, refresh, a filter
   * change), which may spend the forge's rate limit reserve; the interval
   * is not, and coalesced requests keep it. The stage read that follows a
   * list read, or the first one with rows on screen after it, has the
   * list's intent; scrolling reads in the background. */
  listInteractive = false;
  stagesInteractive = false;

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
    this.grant = grantOf(backend, pageGrantsKnown());
    this.connected = getTransportStatusFor(backend).status === 'connected';
    this.stopWatching = $effect.root(() => {
      $effect(() => {
        const grant = grantOf(backend, pageGrantsKnownTracked());
        const connected = getTransportStatusFor(backend).status === 'connected';
        untrack(() => this.observe(grant, connected));
      });
    });
  }

  /** Passive reads run: the computer is connected and granted. */
  get live(): boolean {
    return this.grant === 'granted' && this.connected;
  }

  /** The grant or the connection moved. */
  observe(grant: Grant, connected: boolean): void {
    const was = this.live;
    const grantMoved = grant !== this.grant;
    this.grant = grant;
    this.connected = connected;
    if (grantMoved) syncSelectionHolds(this);
    if (was === this.live) return;
    if (this.live) this.list.request({ immediate: true });
    else this.suspend();
  }

  /** Stops the reads: the timer goes, and the reads in flight are discarded. */
  suspend(): void {
    this.clearTimer();
    this.list.reset();
    this.stageReads.reset();
    // The live edge reads again.
    this.listDeferred = false;
    this.stagesDeferred = false;
    this.loading = false;
  }

  /** The computer was removed: its state goes. A later attach of the
   * same computer reads again on its connection edge. */
  forget(): void {
    this.connected = false;
    this.suspend();
    this.projects = [];
    this.listed = [];
    this.errors.clear();
    this.stages.clear();
    this.loaded = false;
    this.error = null;
    this.readWith = null;
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
      stages: this.stages.get(pipeline.key)?.stages ?? null,
      stagesError: this.stages.get(pipeline.key)?.error ?? null,
    };
  }

  /** Asks for a list read now, as the user's action when interactive. */
  requestList(interactive: boolean): void {
    if (interactive) this.listInteractive = true;
    this.list.request({ immediate: true });
  }

  async read(token: { isCurrent(): boolean }): Promise<void> {
    const explicit = this.explicit;
    this.explicit = false;
    if (!explicit && !this.live) return;
    if (!explicit && documentHidden()) {
      this.listDeferred = true;
      return;
    }
    this.listDeferred = false;
    this.clearTimer();
    this.loading = true;
    const filters = pipelineFilters(this.backend);
    const interactive = this.listInteractive;
    this.listInteractive = false;
    try {
      let projects: ForgeProject[];
      try {
        projects = (await withBackendTarget(this.backend, () => ListForgeProjects())) ?? [];
      } catch (err) {
        if (!token.isCurrent()) return;
        this.error = readError(err);
        return;
      }
      if (!token.isCurrent()) return;
      this.error = null;
      this.projects = projects;
      const picked = projects.filter((project) => filters.projects.length === 0 || filters.projects.includes(project.projectId));
      // Each repository once, however many picked projects clone it.
      const byRepo = new Map<string, { repo: RepoRef; clones: ForgeProject[] }>();
      for (const project of picked) {
        if (project.error) {
          this.errors.set(project.projectId, {
            projectId: project.projectId, name: project.name, message: project.error, failure: forgeFailureFrom({ errorKind: 'forge' }), forge: project.repo.Forge, cause: null,
          });
          continue;
        }
        const repo = repoRefFromWire(project.repo);
        if (!repo) continue;
        const repoId = repoKey(repo);
        const group = byRepo.get(repoId);
        if (group) group.clones.push(project);
        else byRepo.set(repoId, { repo, clones: [project] });
      }
      const reads = await Promise.all([...byRepo].map(async ([repoId, { repo, clones }]) => {
        const lister = clones[0]!;
        try {
          const result = await withBackendTarget(this.backend, () =>
            ListProjectPipelines(lister.projectId, { status: filters.status || undefined }, interactive));
          return { repoId, repo, clones, rows: result?.rows ?? [], error: null };
        } catch (err) {
          return { repoId, repo, clones, rows: null, error: err };
        }
      }));
      if (!token.isCurrent()) return;
      const listed: ListedPipeline[] = [];
      for (const { repoId, repo, clones, rows, error } of reads) {
        if (rows === null) {
          // Every picked clone shows the repository's failure, as the PR
          // list does; the rows listed before stay.
          for (const project of clones) {
            this.errors.set(project.projectId, {
              projectId: project.projectId, name: project.name, message: readError(error), failure: failureOf(error), forge: project.repo.Forge, cause: error,
            });
          }
          listed.push(...this.listed.filter((pipeline) => pipeline.repoId === repoId));
          continue;
        }
        for (const project of clones) this.errors.delete(project.projectId);
        const lister = clones[0]!;
        for (const summary of rows) {
          const key = ciKeyFor({ kind: 'pipeline', backend: this.backend, repo, pipelineId: summary.id });
          listed.push({ key, projectId: lister.projectId, repo, repoId, summary, projectName: lister.name });
        }
      }
      const pickedIds = new Set(picked.map((project) => project.projectId));
      for (const projectId of this.errors.keys()) if (!pickedIds.has(projectId)) this.errors.delete(projectId);
      listed.sort((a, b) => (Date.parse(b.summary.createdAt) || 0) - (Date.parse(a.summary.createdAt) || 0) || Number(b.summary.id) - Number(a.summary.id));
      this.listed = listed;
      this.readWith = filters;
      this.loaded = true;
      // Stages are held for listed rows only, and a stage read issued for
      // the list before is discarded; they re-read with the list.
      const keys = new Set(listed.map((pipeline) => pipeline.key));
      for (const key of this.stages.keys()) if (!keys.has(key)) this.stages.delete(key);
      this.stageReads.reset();
      // They are read as the list's action: the first rows on screen after
      // the user's read are read as theirs.
      this.stagesInteractive = interactive;
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
    if (!this.live) return;
    if (documentHidden()) {
      this.stagesDeferred = true;
      return;
    }
    this.stagesDeferred = false;
    const interactive = this.stagesInteractive;
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
    if (byRepo.size > 0) this.stagesInteractive = false;
    await Promise.all([...byRepo.values()].map(async (batch) => {
      let answer: Record<string, PipelineStages | undefined>;
      try {
        answer = (await withBackendTarget(this.backend, () => ReadPipelineStages(repoReferenceWire(batch.repo), batch.ids, interactive))) ?? {};
      } catch (err) {
        // A dropped connection suspends the page, which reads again.
        if (!token.isCurrent() || isPassiveConnectionFailure(err)) return;
        const failed: StageRead = { stages: null, error: readError(err) };
        for (const key of batch.keys) this.stages.set(key, failed);
        return;
      }
      if (!token.isCurrent()) return;
      batch.ids.forEach((id, index) => {
        const stages = answer[id];
        if (stages) this.stages.set(batch.keys[index]!, stageRead(stages));
      });
    }));
  }

  /** The next list read, while the document is visible: background. */
  arm(): void {
    this.clearTimer();
    if (documentHidden() || this.panes.size === 0 || !this.live) return;
    const wait = Math.max(0, this.lastRead + PIPELINES_REFRESH_MS - Date.now());
    this.timer = setTimeout(() => {
      this.timer = null;
      this.requestList(false);
    }, wait);
  }

  /** The document shows again: a read that came due while hidden runs. */
  shown(): void {
    // The list read arms the next one and reads the stages again.
    if (this.listDeferred) {
      this.list.request({ immediate: true });
      return;
    }
    if (this.stagesDeferred) this.stageReads.request({ immediate: true });
    this.arm();
  }

  clearTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  dispose(): void {
    this.stopWatching();
    this.clearTimer();
    this.list.dispose();
    this.stageReads.dispose();
  }
}

/** A pane's selected pipeline: its CI subject state, and the hold on its
 * subscription while the computer grants it. */
class PipelineSelection {
  hold = $state.raw<PipelineCIAttachment | null>(null);
  readonly ci: CISubjectState;

  constructor(
    readonly backend: BackendKey,
    readonly key: string,
    readonly projectId: string,
    readonly repo: RepoRef,
    readonly pipelineId: string,
    readonly row: PipelineRowView,
  ) {
    this.ci = createCISubjectState(() => ({ kind: 'pipeline', backend, repo, pipelineId }), {
      sourceError: () => {
        if (this.hold) return this.hold.error;
        return pages.get(backend)?.grant === 'refused' ? notGrantedMessage() : null;
      },
    });
  }

  /** Holds the subscription while granted and releases it when not, with
   * the open log, whose follow goes with the subscription. */
  sync(granted: boolean): void {
    if (granted && !this.hold) {
      this.hold = attachPipelineCI(this.backend, this.repo, this.pipelineId);
    } else if (!granted && this.hold) {
      this.ci.closeLogView();
      this.release();
    }
  }

  release(): void {
    const hold = this.hold;
    this.hold = null;
    hold?.release();
  }
}

class PaneEntry {
  selection = $state.raw<PipelineSelection | null>(null);
  onScreen: ReadonlySet<string> = new Set();
  constructor(readonly backend: BackendKey) {}
}

const pages = new SvelteMap<BackendKey, PipelinesPageBackend>();
const paneEntries = new SvelteMap<string, PaneEntry>();

function syncSelectionHolds(page: PipelinesPageBackend): void {
  for (const entry of paneEntries.values()) {
    if (entry.backend === page.backend) entry.selection?.sync(page.grant === 'granted');
  }
}

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
    page.requestList(true);
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

/** The header's refresh and a failed project's retry: reads whatever the
 * connection and grant say, so a refusal shows. */
export function refreshPipelinesPage(paneId: string): void {
  untrack(() => {
    const page = pageOf(paneId);
    if (!page) return;
    page.explicit = true;
    page.requestList(true);
  });
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
    if (page && (!was || was.status !== now.status || was.projects.join('\n') !== now.projects.join('\n'))) page.requestList(true);
  });
}

export function pipelinesRows(paneId: string): readonly PipelineRowView[] {
  return pageOf(paneId)?.rows ?? [];
}

/** Reactive: the first list read has not answered. */
export function pipelinesLoading(paneId: string): boolean {
  const page = pageOf(paneId);
  return page !== null && !page.loaded && pipelinesError(paneId) === null;
}

/** Reactive: a list read (open, refresh or the interval) is in flight. */
export function pipelinesReading(paneId: string): boolean {
  return pageOf(paneId)?.loading ?? false;
}

/** Reactive: the projects could not be read, or the computer does not
 * grant the reads, so nothing lists. */
export function pipelinesError(paneId: string): string | null {
  const page = pageOf(paneId);
  if (!page) return null;
  return page.error ?? (page.grant === 'refused' ? notGrantedMessage() : null);
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
    const selection = new PipelineSelection(backend, key, listed.projectId, repo, pipelineId, page.view(listed));
    selection.sync(page.grant === 'granted');
    entry.selection = selection;
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
    selection.release();
  });
}

/** The compact probe: a pipeline's detail is open above the list. */
export function pipelineDetailOpen(paneId: string): boolean {
  return (paneEntries.get(paneId)?.selection ?? null) !== null;
}

onBackendDetached(({ backendId }) => {
  untrack(() => {
    const page = pages.get(backendId);
    if (!page) return;
    for (const [paneId, entry] of paneEntries) if (entry.backend === backendId) clearPipelineSelection(paneId);
    page.forget();
  });
});

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    for (const page of pages.values()) {
      if (documentHidden()) {
        page.clearTimer();
        continue;
      }
      page.shown();
    }
  });
}

/** Tests: the row keys whose stage reads the pane's page holds. */
export function __pipelineStageKeysForTest(paneId: string): string[] {
  return [...(pageOf(paneId)?.stages.keys() ?? [])];
}

export function __resetPipelinesPageForTest(): void {
  for (const paneId of paneEntries.keys()) detachPipelinesPage(paneId);
}
