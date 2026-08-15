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
  /** Thread the row created; empty when the session had no importable history. */
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
 * One entry of the project dropdown — one AO PROJECT, not one cwd.
 *
 * Membership survives the provider, project and query filters — picking a
 * project must not empty the menu it was picked in — but NOT the already-ran
 * toggle: a project whose every row is withheld from the offer has nothing to
 * pick. `count` is the number of rows that survive the current provider +
 * query filters, so a listed group can still show zero.
 */
export interface ImportProjectGroup {
  /**
   * What the project filter stores: `ImportableSession.projectId` when the
   * rows resolve to a project AO already has, else the cwd. A repo root and
   * its subdirectories are one project and therefore one entry — keying on
   * the raw cwd listed the same project several times over.
   */
  key: string;
  /**
   * Representative cwd — the shortest member path. The project's own root is
   * not on the row (only the session's cwd is), so this is the closest thing
   * to it the catalog knows, and it is display-only.
   */
  path: string;
  /** Project name for a known project, else the cwd's base name. */
  label: string;
  count: number;
  /** The rows resolve to a project AO already has (`key` is its id). */
  known: boolean;
}

/** A row's outcome inside a run, folded from progress frames. */
export interface ImportRowResult {
  id: string;
  status: ImportRowStatus;
  threadIds: string[];
  /** User-facing prose; "" unless the row failed or was skipped. */
  error: string;
}
