// Self-update store. Owns the reactive updater state shared by the Settings →
// Updates panel and the sidebar "update available" badge, and bridges the
// backend updater:* events into that state.
//
// UX contract (mirrors the backend in app_updater.go): the on-launch check only
// reads release metadata — nothing downloads or installs without an explicit
// button press. The download and the restart are each a separate user action.

import {
  CheckForUpdate,
  ListReleases,
  DownloadUpdate,
  RestartToUpdate,
} from './bindings';
import type { ReleaseSummary } from './bindings';
import { wailsEventOn } from './wailsEvents';
import { userFacingError } from '../utils/userFacingError';

export type { ReleaseSummary };

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

  // Version selection (the "Advanced" disclosure). Lazily populated by
  // loadVersions the first time the user expands it — the common path only
  // ever installs the latest and never pays for this list.
  availableVersions: [] as ReleaseSummary[],
  versionsLoaded: false,
  versionsLoading: false,
  versionsError: '',
  // selectedTag is the release the Advanced picker will install. '' until the
  // list loads, then defaulted to the latest stable. A specific (possibly
  // older) tag here drives a by-tag download — the rollback path.
  selectedTag: '',
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
 * loadVersions fetches the installable releases for the Advanced version picker.
 * Lazy: call it the first time the user expands the disclosure. Read-only — it
 * never downloads or installs. On success it defaults the selection to the
 * latest stable so the picker opens on the safe choice.
 */
export async function loadVersions(): Promise<void> {
  if (!state.supported || state.versionsLoading) return;
  state.versionsLoading = true;
  state.versionsError = '';
  try {
    const releases = await ListReleases();
    state.availableVersions = releases;
    state.versionsLoaded = true;
    // Default (or re-anchor) the selection to the latest stable. Re-anchor only
    // when the current pick is gone, so a reload doesn't clobber a deliberate
    // selection that's still valid.
    if (!releases.some((r) => r.tag === state.selectedTag)) {
      state.selectedTag = releases.find((r) => r.isLatest)?.tag ?? releases[0]?.tag ?? '';
    }
  } catch (err) {
    state.versionsError = userFacingError(err, 'Could not load available versions.');
  } finally {
    state.versionsLoading = false;
  }
}

/** selectVersion sets which release the Advanced picker will install. */
export function selectVersion(tag: string): void {
  state.selectedTag = tag;
}

/** selectedVersion returns the currently-selected release summary, if any. */
export function selectedVersion(): ReleaseSummary | undefined {
  return state.availableVersions.find((r) => r.tag === state.selectedTag);
}

/**
 * canInstallSelected reports whether the Advanced "Install" action is valid for
 * the current selection: a real, non-current release while nothing else is in
 * flight. Reinstalling the running version is a no-op, so it's disallowed.
 */
export function canInstallSelected(): boolean {
  const v = selectedVersion();
  if (!v || v.isCurrent) return false;
  return !isDownloadInFlight(state.phase) && state.phase !== 'checking' && state.phase !== 'ready';
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
 * startUpdateDownload downloads, verifies, and stages a release. The backend
 * runs the work asynchronously and reports progress + the terminal state via
 * updater:* events, which this store bridges into `state`.
 *
 * tag === '' installs the pending latest (the passive-check result) and is only
 * valid from 'available'. A specific tag installs that exact release — possibly
 * an OLDER one (rollback) — and is valid from any resting phase, since the
 * backend resolves it on demand rather than relying on a staged pending release.
 */
export async function startUpdateDownload(tag = ''): Promise<void> {
  if (!state.supported) return;
  if (tag === '') {
    if (state.phase !== 'available') return;
  } else if (
    isDownloadInFlight(state.phase) ||
    state.phase === 'checking' ||
    state.phase === 'ready'
  ) {
    return;
  }
  state.error = '';
  state.written = 0;
  state.total = 0;
  // Flip to downloading immediately so the button can't be double-fired; the
  // backend confirms via updater:download-started / updater:progress.
  state.phase = 'downloading';
  try {
    await DownloadUpdate(tag);
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
  state.availableVersions = [];
  state.versionsLoaded = false;
  state.versionsLoading = false;
  state.versionsError = '';
  state.selectedTag = '';
  initialized = false;
}
