import type { BackendKey } from '../transport/backendKey';
import type { PRListSnapshot } from './bindings';
import type { CIPipeline } from '../types/models';
import type { ForgeFailureWire } from '../utils/forgeFailure';
import type { PRCILogEvent } from './prReviewCI.svelte';

// The Pull requests page's backend frames, as eventsPulls.ts hands them
// over (docs/architecture/forge-transport.md, "PR list pump" and
// "Pipeline pump"). The page's store owns what each one does; until it
// subscribes nothing emits them, and these receive nothing.

/** The "ci:pipeline_updated" frame. Wails generates no TS type for event
 * payloads; kept in sync with PipelineUpdatedEvent in
 * internal/app/app_pipeline_pump.go. */
export interface PipelineUpdatedEvent extends ForgeFailureWire {
  key: string;
  /** Set on a pipeline frame. */
  pipeline?: CIPipeline | null;
  /** Set on an error frame; the pipeline shown before stays. */
  error?: string;
  seq: number;
}

/** A "pr:ci_log" frame for a job a pipeline subscription follows. */
export type PipelineCILogEvent = PRCILogEvent & { pipelineKey: string };

type PullsReceiver = {
  pulls?: (snapshot: PRListSnapshot, backend: BackendKey) => void;
  pipeline?: (event: PipelineUpdatedEvent, backend: BackendKey) => void;
  pipelineLog?: (event: PipelineCILogEvent, backend: BackendKey) => void;
  pullsGap?: (backend: BackendKey) => Promise<void> | undefined;
  pipelinesGap?: (backend: BackendKey) => Promise<void> | undefined;
};

let receiver: PullsReceiver = {};

/** Installs the page store's handlers, replacing any before; the returned
 * function removes them. */
export function setPullsReceiver(next: PullsReceiver): () => void {
  receiver = next;
  return () => {
    if (receiver === next) receiver = {};
  };
}

export function applyPullsUpdatedEvent(snapshot: PRListSnapshot, backend: BackendKey): void {
  receiver.pulls?.(snapshot, backend);
}

export function applyPipelineUpdatedEvent(event: PipelineUpdatedEvent, backend: BackendKey): void {
  receiver.pipeline?.(event, backend);
}

export function applyPipelineCILogEvent(event: PipelineCILogEvent, backend: BackendKey): void {
  receiver.pipelineLog?.(event, backend);
}

/** A pulls:updated gap: the next frame may be a change away from the
 * snapshot held, so the list is read again. Answers the read's settling,
 * or undefined when none started. */
export function resyncPullsAfterGap(backend: BackendKey): Promise<void> | undefined {
  return receiver.pullsGap?.(backend);
}

/** A ci:pipeline_updated or pipeline pr:ci_log gap: every subscribed
 * pipeline and followed log is read again. */
export function resyncPipelinesAfterGap(backend: BackendKey): Promise<void> | undefined {
  return receiver.pipelinesGap?.(backend);
}
