// Session-import wire shapes.
//
// The Go structs are generated into `bindings/agent-overflow/models`, so
// every shape that exists there is re-exported from there and NOT restated
// here — same arrangement as `types/git.ts`. Feature code imports from this
// file rather than the generated tree; nothing outside this file may
// re-declare one of these shapes.
//
// What stays hand-written is what the generator does not emit: the
// `session-import:progress` payload (an event, not a method signature or
// return type, so no binding is produced for it) and the frontend-only view
// types the modal projects the catalog into.
//
// All timestamps are epoch MILLISECONDS.

export type {
  ImportProviderStatus,
  ImportRunHandle,
  ImportScanRequest,
  ImportScanResult,
  ImportSessionsRequest,
  ImportUpdateResult,
  ImportUpdateStatus,
  ImportableSession,
} from '../../../bindings/agent-overflow/models';

/** Providers that can be imported from. */
export type ImportProvider = 'claude' | 'codex';

/** Per-row outcome reported on a progress frame. */
export type ImportRowStatus = 'imported' | 'failed' | 'skipped';

/**
 * The one narrowing for `ImportRowStatus`. Both the event handler (which
 * rejects a whole frame carrying an unknown status) and the store's fold
 * (which refuses to stamp one onto a row) need it, and two copies of a set
 * membership test are two places for the set to drift from the type above.
 */
export function isRowStatus(value: unknown): value is ImportRowStatus {
  return value === 'imported' || value === 'failed' || value === 'skipped';
}

/**
 * `session-import:progress` payload (Go: `SessionImportProgressEvent`).
 *
 * Hand-written because Wails only generates models reachable from a bound
 * method's signature, and this one is only ever emitted. ONE channel carries
 * both the per-row frames and the terminal frame; the terminal frame sets
 * `done` and omits the per-row fields.
 */
export interface SessionImportProgressEvent {
  importId: string;
  /**
   * Frames reported so far, monotonic, 0..total. A CANCELLED run's terminal
   * frame stops short of `total` — that shortfall is how a stopped run is
   * told apart from a finished one, so nothing may round it up.
   */
  completed: number;
  total: number;
  /** Row id this frame reports on; absent on the terminal frame. */
  id?: string;
  status?: ImportRowStatus;
  /** Threads the row actually created — the real branch count. */
  threadIds?: string[];
  /** User-facing prose; set on `failed` AND on `skipped`. */
  error?: string;
  /** True exactly once, on the final frame. */
  done?: boolean;
}

/**
 * Provider-update check verdicts. The wire field is a plain Go string (the
 * generated `ImportUpdateStatus.status`), so this is the set the backend
 * emits, not a guarantee — consumers must still carry a default branch.
 */
export type ImportUpdateStatusKind =
  | 'up-to-date'
  | 'updates-available'
  | 'diverged-local'
  | 'source-missing'
  | 'source-diverged'
  | 'not-imported';

// ---------------------------------------------------------------------------
// Frontend-only view types
// ---------------------------------------------------------------------------

/** Provider segment of the import toolbar. */
export type ImportProviderFilter = 'all' | ImportProvider;

/**
 * One entry of the project dropdown. Built from the FULL row set so the
 * dropdown doesn't shrink as other filters narrow; `count` is the number of
 * rows that survive the current provider + query filters, so a group can
 * legitimately show zero.
 */
export interface ImportProjectGroup {
  /** `ImportableSession.projectPath` — the group key. */
  path: string;
  /** Backend-computed `projectLabel` of the first row in the group. */
  label: string;
  count: number;
}

/** A row's outcome inside a run, folded from progress frames. */
export interface ImportRowResult {
  id: string;
  status: ImportRowStatus;
  threadIds: string[];
  /** User-facing prose; "" unless the row failed or was skipped. */
  error: string;
}
