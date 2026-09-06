// session-import:progress event domain: per-row outcomes and the terminal
// frame of an import run. Fan-in target of events.ts's setupEventListeners.
//
// The channel reaches any client whose session holds `threads:operate` (the
// grant ListImportableSessions and ImportSessions already take), and this
// handler validates every field regardless: the store's fold is what drives a
// progress bar, a close, and a toast that claims threads were created, and a
// malformed frame must not be able to claim any of that. Same posture as
// events.ts's system:stats guard — reject the frame, never partially accept
// it.
//
// A dropped socket is deliberately NOT a terminal condition here. The
// transport replays every channel's missed frames on reconnect
// (wsClient.ts's replay handshake against the server's per-channel ring),
// and `session-import:progress` is a retained channel — so a blip mid-run
// ends with the missed frames, terminal `done` included, arriving late. The
// run stays active across it and settles on the replay, the same way
// eventsWorktreeSetup and eventsProvider recover. Ending the run on the
// status transition would drop exactly those replayed frames on the store's
// `if (!current.active) return`, leaving the modal reading "Connection
// lost" over a run the backend finished.
//
// The real proof of loss is the transport GAP signal — the server's ring
// could not cover this client's cursor — and that arrives on
// `eventsTransportGap.ts`, which is where `markImportConnectionLost` is
// called from.

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { backendKeyForOrigin } from '../transport/backends';
import { onBackendStatusChange } from './transportStatus.svelte';
import type { ImportRowStatus, SessionImportProgressEvent } from '../types/sessionImport';
import { isRowStatus } from '../types/sessionImport';
import { applyImportProgress, getSessionImportRun, getSessionImportBackend } from './sessionImport.svelte';
import { wailsEventOn } from './wailsEvents';

/** Bounds the user-facing prose a frame can carry into the row stamps. */
const IMPORT_ERROR_MAX_CHARS = 4_000;
/** Bounds ids (row keys and thread ids) so a frame can't blow up the map. */
const IMPORT_ID_MAX_CHARS = 512;
/** Bounds the thread list one row can report. */
const IMPORT_THREAD_IDS_MAX = 10_000;

function isId(value: unknown, max = IMPORT_ID_MAX_CHARS): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= max;
}

function isCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

/**
 * Validate one frame and hand it to the store.
 *
 * Returns whether the frame was accepted so the wiring can log a rejected
 * one — a silently dropped frame here would show up as a progress bar that
 * stopped moving with no explanation anywhere.
 */
export function applySessionImportProgress(payload: unknown, backend: BackendKey = HOME_BACKEND): boolean {
  if (!payload || typeof payload !== 'object') return false;
  const evt = payload as Record<string, unknown>;

  if (!isId(evt.importId)) return false;
  if (!isCount(evt.completed) || !isCount(evt.total)) return false;
  if (evt.done !== undefined && typeof evt.done !== 'boolean') return false;

  // The per-row fields travel together: an id without a status (or the
  // reverse) is a frame this client cannot attribute, and attributing it to
  // the wrong row would stamp a lie onto the list.
  const hasId = evt.id !== undefined && evt.id !== '';
  const hasStatus = evt.status !== undefined && evt.status !== '';
  if (hasId !== hasStatus) return false;
  if (hasId && (!isId(evt.id) || !isRowStatus(evt.status))) return false;

  if (evt.error !== undefined && (typeof evt.error !== 'string' || evt.error.length > IMPORT_ERROR_MAX_CHARS)) {
    return false;
  }

  let threadIds: string[] | undefined;
  if (evt.threadIds !== undefined && evt.threadIds !== null) {
    if (!Array.isArray(evt.threadIds) || evt.threadIds.length > IMPORT_THREAD_IDS_MAX) return false;
    if (!evt.threadIds.every((id) => isId(id))) return false;
    threadIds = evt.threadIds as string[];
  }

  // Rebuilt rather than forwarded: the store keeps what it is handed, and a
  // frame carrying extra keys should not smuggle them into run state.
  const frame: SessionImportProgressEvent = {
    importId: evt.importId,
    completed: Math.trunc(evt.completed),
    total: Math.trunc(evt.total),
    done: evt.done === true,
  };
  if (hasId) {
    frame.id = evt.id as string;
    frame.status = evt.status as ImportRowStatus;
  }
  if (typeof evt.error === 'string' && evt.error !== '') frame.error = evt.error;
  if (threadIds) frame.threadIds = threadIds;

  applyImportProgress(frame, backend);
  return true;
}

/**
 * Subscribe to the progress channel. Returns the teardown.
 *
 * The second subscription is a diagnostic breadcrumb, not control flow: a
 * run whose frames stop mid-import is otherwise indistinguishable from a
 * backend that stalled, and the drop is the explanation. It records the
 * drop and changes nothing — recovery is the transport's replay, and the
 * unrecoverable case is the gap signal (see the module header).
 */
export function setupSessionImportEvents(): () => void {
  const cancelProgress = wailsEventOn<unknown>('session-import:progress', (payload, origin) => {
    if (!applySessionImportProgress(payload, backendKeyForOrigin(origin.backendId))) {
      console.warn('events: session-import:progress dropped a malformed frame', payload);
    }
  });
  const cancelStatus = onBackendStatusChange((backend, snapshot) => {
    if (backend !== getSessionImportBackend() || snapshot.status === 'connected') return;
    const run = getSessionImportRun();
    if (!run?.active) return;
    console.info(
      `events: transport dropped during import ${run.importId} ` +
        `(${run.completed}/${run.total}) — awaiting replay on reconnect`,
    );
  });
  return () => {
    cancelProgress();
    cancelStatus();
  };
}
