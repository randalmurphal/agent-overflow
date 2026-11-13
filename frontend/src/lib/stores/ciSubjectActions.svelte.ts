// One CI subject's actions as its surfaces read them: the job and
// pipeline actions the read's flags show, the one pending on the
// pipeline, and the artifact downloads (ciActions.svelte.ts owns the
// pending rule). The CI subject state builds one per surface over its
// subject, so the review pane and the Pipelines tab run the same code.

import { SaveCIArtifacts } from './bindings';
import {
  ciArtifactsDownloading,
  ciArtifactsTarget,
  ciJobActionKinds,
  ciJobHasArtifacts,
  ciPipelineActionKinds,
  ciPipelineHasArtifacts,
  peekCIAction,
  runCIAction,
  trackCIArtifactsDownload,
  type CIActionKind,
  type CIJobActionKind,
  type CIPendingAction,
  type CIPipelineActionKind,
} from './ciActions.svelte';
import { addErrorToast } from './toast.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { CIJob, CIPipeline } from '../types/models';
import { errString } from '../utils/errors';
import type { RepoReferenceWire } from '../utils/prReference';

/** A GitHub artifacts download names at most this many workflow runs
 * (git.CIArtifactsRequest). */
const MAX_ARTIFACT_RUNS = 20;

export interface CIArtifactsSavedView {
  readonly dir: string;
  readonly paths: readonly string[];
}

export interface CIActionsView {
  /** The action pending on the pipeline; null with none. */
  readonly pending: CIPendingAction | null;
  readonly pipelineActions: readonly CIPipelineActionKind[];
  /** A GitHub pipeline's artifacts can be downloaded. */
  readonly pipelineHasArtifacts: boolean;
  /** The sentence saying why some actions are not offered (the read's
   * actionsUnavailable); null when every flag was read. */
  readonly actionsUnavailable: string | null;
  jobActions(job: CIJob): readonly CIJobActionKind[];
  /** A GitLab job's archive can be downloaded. */
  jobHasArtifacts(job: CIJob): boolean;
  /** Whether `kind` on `job` (null: the pipeline) is the pending action. */
  isPending(kind: CIActionKind, job: CIJob | null): boolean;
  /** The download of a job's archive (null: the pipeline's) runs. */
  artifactsDownloading(job: CIJob | null): boolean;
  run(kind: CIActionKind, job: CIJob | null): Promise<boolean>;
  /** Downloads a job's archive, or the pipeline's artifacts with null;
   * answers what was saved, or null with the failure toasted. */
  saveArtifacts(job: CIJob | null): Promise<CIArtifactsSavedView | null>;
}

export interface CIActionsContext {
  readonly key: string | null;
  readonly backend: BackendKey | null;
  readonly repo: RepoReferenceWire | null;
  readonly pipeline: CIPipeline | null;
}

/** The run ids a GitHub pipeline's artifacts download names: the run a
 * Pipelines-tab pipeline is, or the distinct runs of a PR's checks. */
export function ciArtifactRunIds(pipeline: CIPipeline | null): string[] {
  if (!pipeline) return [];
  if (pipeline.id) return [pipeline.id];
  const ids = new Set<string>();
  for (const stage of pipeline.stages) {
    for (const job of stage.jobs) if (job.runId) ids.add(job.runId);
  }
  return [...ids].slice(0, MAX_ARTIFACT_RUNS);
}

export function createCIActionsView(
  context: () => CIActionsContext,
  onSaved: (job: CIJob | null, saved: CIArtifactsSavedView) => void,
): CIActionsView {
  const at = $derived(context());
  const pending = $derived(peekCIAction(at.key));

  return {
    get pending() { return pending; },
    get pipelineActions() { return ciPipelineActionKinds(at.pipeline); },
    get pipelineHasArtifacts() { return ciPipelineHasArtifacts(at.pipeline) && ciArtifactRunIds(at.pipeline).length > 0; },
    get actionsUnavailable() { return at.pipeline?.actionsUnavailable || null; },
    jobActions: ciJobActionKinds,
    jobHasArtifacts: ciJobHasArtifacts,

    isPending(kind, job) {
      return pending !== null && pending.kind === kind && pending.jobId === (job?.id ?? null);
    },

    artifactsDownloading(job) {
      return ciArtifactsDownloading(at.key, ciArtifactsTarget(job?.id ?? null));
    },

    async run(kind, job) {
      const { key, backend } = at;
      if (key === null || backend === null) return false;
      return runCIAction(key, backend, kind, job?.id ?? null);
    },

    async saveArtifacts(job) {
      const { key, backend, repo, pipeline } = at;
      if (key === null || backend === null || repo === null) return null;
      const jobId = job?.id ?? null;
      const request = jobId !== null ? { jobId } : { runIds: ciArtifactRunIds(pipeline) };
      if (jobId === null && request.runIds?.length === 0) return null;
      try {
        const saved = await trackCIArtifactsDownload(key, ciArtifactsTarget(jobId), () =>
          withBackendTarget(backend, () => SaveCIArtifacts(repo, request, job?.name ?? '')));
        if (!saved) return null;
        const view: CIArtifactsSavedView = { dir: String(saved.dir), paths: (saved.paths ?? []).map(String) };
        onSaved(job, view);
        return view;
      } catch (err) {
        addErrorToast(errString(err), err);
        return null;
      }
    },
  };
}
