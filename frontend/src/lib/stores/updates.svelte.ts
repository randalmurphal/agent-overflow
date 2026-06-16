// Self-update store. Owns the reactive updater state shared by the Settings →
// Updates panel and the sidebar "update available" badge, and bridges the
// backend updater:* events into that state.
//
// UX contract (mirrors the backend in app_updater.go): the on-launch check only
// reads release metadata — nothing downloads or installs without an explicit
// button press. The download and the restart are each a separate user action.

import {
  CheckForUpdate,
  DownloadUpdate,
  RestartToUpdate,
} from './bindings';
import { wailsEventOn } from './wailsEvents';
import { userFacingError } from '../utils/userFacingError';

export type UpdaterPhase =
  | 'idle'
  | 'checking'
  | 'available'
  | 'up-to-date'
  | 'downloading'
  | 'verifying'
  | 'installing'
  | 'ready'
  | 'error';

interface ProgressPayload {
  written?: number;
  total?: number;
  rate?: number;
}

interface ErrorPayload {
  stage?: string;
  message?: string;
}

const state = $state({
  // supported is false on builds that can't self-update (headless WSL backend,
  // dev builds); the UI hides the section and the badge stays dark.
  supported: true,
  phase: 'idle' as UpdaterPhase,
  currentVersion: '',
  latestVersion: '',
  releaseName: '',
  releaseNotes: '',
  written: 0,
  total: 0,
  error: '',
});

export type UpdateState = typeof state;

/** getUpdateState returns the live reactive updater state. */
export function getUpdateState(): UpdateState {
  return state;
}

/**
 * hasPendingUpdate reports whether a newer version has been found and not yet
 * installed+restarted. Drives the sidebar badge. A successful restart relaunches
 * into the new version, whose on-launch check returns up-to-date and clears
 * latestVersion, so the badge naturally goes dark.
 */
export function hasPendingUpdate(): boolean {
  return state.supported && state.latestVersion !== '';
}

/** isDownloadInFlight reports whether the phase is one of the active
 * download/verify/install steps — the UI shows the progress bar and blocks a
 * new check/download while any of them is current. */
export function isDownloadInFlight(phase: UpdaterPhase): boolean {
  return phase === 'downloading' || phase === 'verifying' || phase === 'installing';
}

/**
 * runUpdateCheck asks the backend whether a newer release exists. Read-only:
 * it never downloads or installs. Safe to call repeatedly; overlapping or
 * mid-download calls are ignored.
 */
export async function runUpdateCheck(): Promise<void> {
  // Skip if a check is already running or a download is in flight. Also skip
  // when an update is already staged ('ready'): a re-check would re-resolve the
  // same release and flip the phase back to 'available', dropping the user from
  // "Restart to update" to "Download" even though the staged build is still
  // valid and waiting. Every branch below sets a terminal phase, so no overlap
  // flag or finally is needed.
  if (state.phase === 'checking' || state.phase === 'ready' || isDownloadInFlight(state.phase)) {
    return;
  }
  state.phase = 'checking';
  state.error = '';
  try {
    const result = await CheckForUpdate();
    state.supported = result.supported;
    state.currentVersion = result.currentVersion;
    if (!result.supported) {
      state.phase = 'idle';
      state.latestVersion = '';
      return;
    }
    if (result.available) {
      state.latestVersion = result.latestVersion ?? '';
      state.releaseName = result.releaseName ?? '';
      state.releaseNotes = result.releaseNotes ?? '';
      state.phase = 'available';
    } else {
      state.latestVersion = '';
      state.releaseName = '';
      state.releaseNotes = '';
      state.phase = 'up-to-date';
    }
  } catch (err) {
    state.phase = 'error';
    state.error = userFacingError(err, 'Could not check for updates.');
  }
}

/**
 * startUpdateDownload downloads, verifies, and stages the pending release. The
 * backend runs the work asynchronously and reports progress + the terminal
 * state via updater:* events, which this store bridges into `state`.
 */
export async function startUpdateDownload(): Promise<void> {
  if (!state.supported || state.phase !== 'available') return;
  state.error = '';
  state.written = 0;
  state.total = 0;
  // Flip to downloading immediately so the button can't be double-fired; the
  // backend confirms via updater:download-started / updater:progress.
  state.phase = 'downloading';
  try {
    await DownloadUpdate();
  } catch (err) {
    state.phase = 'error';
    state.error = userFacingError(err, 'Could not start the update download.');
  }
}

/**
 * restartForUpdate swaps in the staged update and relaunches. This quits the
 * running app, so it is only ever wired to an explicit button.
 */
export async function restartForUpdate(): Promise<void> {
  if (state.phase !== 'ready') return;
  try {
    await RestartToUpdate();
    // The app is now shutting down; the swap helper relaunches the new version.
  } catch (err) {
    state.phase = 'error';
    state.error = userFacingError(err, 'Could not restart to apply the update.');
  }
}

let initialized = false;

/**
 * initUpdates subscribes to the backend updater event channels and kicks off
 * the passive on-launch check. Idempotent; returns a cleanup function that
 * tears down the subscriptions. Call once from the app root.
 */
export function initUpdates(): () => void {
  if (initialized) return () => {};
  initialized = true;

  const cancels = [
    wailsEventOn('updater:download-started', () => {
      state.phase = 'downloading';
    }),
    wailsEventOn<ProgressPayload>('updater:progress', (p) => {
      state.phase = 'downloading';
      state.written = p?.written ?? 0;
      state.total = p?.total ?? 0;
    }),
    wailsEventOn('updater:verifying', () => {
      state.phase = 'verifying';
    }),
    wailsEventOn('updater:installing', () => {
      state.phase = 'installing';
    }),
    wailsEventOn('updater:ready', () => {
      state.phase = 'ready';
    }),
    wailsEventOn<ErrorPayload>('updater:error', (e) => {
      state.phase = 'error';
      const stage = e?.stage ? `${e.stage} ` : '';
      state.error = e?.message ? `Update ${stage}failed: ${e.message}` : 'Update failed.';
    }),
  ];

  // Passive on-launch check — surfaces availability without installing anything.
  void runUpdateCheck();

  return () => {
    for (const cancel of cancels) cancel();
    initialized = false;
  };
}

/**
 * resetForTest restores the singleton store to its initial state and clears the
 * initialized guard so each test starts clean. Test-only seam (mirrors the
 * other runes stores, e.g. providerStatus).
 */
export function resetForTest(): void {
  state.supported = true;
  state.phase = 'idle';
  state.currentVersion = '';
  state.latestVersion = '';
  state.releaseName = '';
  state.releaseNotes = '';
  state.written = 0;
  state.total = 0;
  state.error = '';
  initialized = false;
}
