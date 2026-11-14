// Self-update store. Owns the reactive updater state shared by the Settings →
// Updates panel and the sidebar's Update button, and bridges the backend
// updater:* events into that state.
//
// UX contract (mirrors the backend in internal/appupdate): a check only reads
// release metadata — nothing downloads or installs without an explicit button
// press. The download and the restart are each a separate user action.
//
// The store checks at launch and then every UPDATE_CHECK_INTERVAL_MS while the
// page is open. The repeating check is a background check: it never shows the
// 'checking' phase, and a release source it cannot reach leaves the state as
// it was, unless that state offered a download the backend can no longer run.

import {
  CheckForUpdate,
  ListReleases,
  DownloadUpdate,
  RestartToUpdate,
  CancelRestartToUpdate,
} from './bindings';
import type { ReleaseSummary } from './bindings';
import { wailsEventOn } from './wailsEvents';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { isMethodUnavailableError } from './transportStatus.svelte';
import { userFacingError } from '../utils/userFacingError';
import { hasPendingServiceUpdate } from './serviceUpdate.svelte';

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
  // 'waiting' is a requested restart the backend holds until this host's
  // running work finishes (updater:restart names what it waits for). Cancel
  // returns to 'ready'.
  | 'waiting'
  // 'restarting' latches after a successful RestartToUpdate call. On desktop
  // the process quits moments later, but on WSL the RPC returns as soon as the
  // directive is handed to the Windows launcher, which then spends up to two
  // minutes verifying and swapping while this app stays alive. The phase keeps
  // the Restart button (and every other update action) out of reach in that
  // window; the backend's terminal updater:error moves us to 'error' if the
  // handoff fails, and success kills the process.
  | 'restarting'
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

// updater:restart, the backend's RestartUpdateStatus.
interface RestartPayload {
  phase?: 'waiting' | 'restarting' | 'canceled' | 'failed' | 'idle';
  waitingFor?: string;
  error?: string;
}

const restartFailedMessage = 'Could not restart to apply the update.';
const checkFailedMessage = 'Could not check for updates.';

/** How often an open page asks the release source again after the launch check. */
export const UPDATE_CHECK_INTERVAL_MS = 60 * 60 * 1000;

const state = $state({
  // supported is false on builds that can't self-update (dev builds) and on
  // remote sessions, where the updater RPCs are `host`-scoped and no grant
  // reaches them; the UI hides the section and the badge stays dark.
  supported: true,
  phase: 'idle' as UpdaterPhase,
  currentVersion: '',
  latestVersion: '',
  releaseName: '',
  releaseNotes: '',
  written: 0,
  total: 0,
  error: '',
  // waitingFor is what a 'waiting' restart waits for, in the backend's words.
  waitingFor: '',
  canceling: false,
  cancelError: '',
  // lastApplyFailure mirrors the backend's boot-detected notice that the
  // PREVIOUS session's staged update never got applied. It is backend-owned
  // process-lifetime state, so this store copies whatever each check returns
  // rather than latching it: that is what makes the notice both survive
  // re-checks (the backend keeps returning it) and disappear the moment a
  // check stops reporting it. Unlike `error` it is not a phase — it stays
  // visible through checking / available / downloading alike.
  lastApplyFailure: '',

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
 * installed+restarted. Drives the sidebar's Update button and the dot on the
 * Updates tab. A successful restart relaunches into the new version, whose
 * launch check returns up-to-date and clears latestVersion, so both go away.
 *
 * Both also show for a SUPERVISED machine this client is attached to with a
 * newer release waiting (`serviceUpdate.svelte.ts`): the two updaters are
 * different mechanisms, but "something on Settings → Updates wants you" is
 * one fact, and both sites ask it here once.
 */
export function hasPendingUpdate(): boolean {
  return (state.supported && state.latestVersion !== '') || hasPendingServiceUpdate();
}

// errorFromCheck is whether the current 'error' phase came from a release
// check. A background check may replace that error with its own answer. An
// error from a download or a restart stays until the user acts on it. fail is
// the only writer of the 'error' phase, so the two cannot disagree.
let errorFromCheck = false;

function fail(message: string, fromCheck = false): void {
  state.phase = 'error';
  state.error = message;
  errorFromCheck = fromCheck;
}

/** isDownloadInFlight reports whether the phase is one of the active
 * download/verify/install steps — the UI shows the progress bar and blocks a
 * new check/download while any of them is current. */
export function isDownloadInFlight(phase: UpdaterPhase): boolean {
  return phase === 'downloading' || phase === 'verifying' || phase === 'installing';
}

/**
 * isUpdateFlowBusy reports whether the update flow is mid-action: a check
 * running, a download/verify/install in flight, an update staged ready (a
 * re-check or second download would clobber the staged release), or a restart
 * handoff under way. Every action entry point — check, download, the Advanced
 * install, and the settings Check button — gates on this one predicate, so a
 * new phase joins the set here once instead of at each call site.
 */
export function isUpdateFlowBusy(phase: UpdaterPhase): boolean {
  return (
    phase === 'checking' ||
    phase === 'ready' ||
    phase === 'waiting' ||
    phase === 'restarting' ||
    isDownloadInFlight(phase)
  );
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
  return !isUpdateFlowBusy(state.phase);
}

/**
 * runUpdateCheck asks the backend whether a newer release exists. Read-only:
 * it never downloads or installs. Safe to call repeatedly; mid-flow calls are
 * ignored and the newest of overlapping checks wins.
 */
export function runUpdateCheck(): Promise<void> {
  return startCheck(false);
}

// newestCheck settles once the newest check has applied or dropped its answer.
let newestCheck: Promise<void> = Promise.resolve();

function startCheck(background: boolean): Promise<void> {
  newestCheck = check(background);
  return newestCheck;
}

// checkSeq numbers the checks so a reply applies only while its check is the
// newest one.
let checkSeq = 0;

/**
 * check runs one release check. A background check is the repeating one
 * nobody asked for: it leaves the phase alone while it runs, and a release
 * source it cannot reach is not reported.
 */
async function check(background: boolean): Promise<void> {
  // The updater RPCs are `host`-scoped, and this runs unprompted. Off-host
  // the call can only be refused, so ask first and land in the same resting
  // state the refusal would have produced — the reactive catch below stays as
  // the backstop for a refusal nobody predicted.
  // The answer lands with the bootstrap manifest, AFTER App mounts and the
  // launch check starts; reading it earlier marked updates unsupported on
  // every launch of the merged build.
  await pageGrantsResolved();
  // Skip while the flow is mid-action. The 'ready' member of that set matters
  // most here: a re-check would re-resolve the same release and flip the phase
  // back to 'available', dropping the user from "Restart to update" to
  // "Download" even though the staged build is still valid and waiting.
  if (isUpdateFlowBusy(state.phase)) {
    return;
  }
  // The error of a download or a restart is the user's to read.
  if (background && state.phase === 'error' && !errorFromCheck) {
    return;
  }
  if (!hasScope('host')) {
    markUnsupported();
    return;
  }
  if (!background) {
    state.phase = 'checking';
    state.error = '';
  }
  const seq = ++checkSeq;
  const startedIn = state.phase;
  // A reply is stale once a newer check started or the phase moved on under
  // it: a download the user started, or a flow event from the backend. What
  // moved the phase owns it.
  const stale = () => seq !== checkSeq || state.phase !== startedIn;
  try {
    const result = await CheckForUpdate();
    if (stale()) return;
    state.currentVersion = result.currentVersion;
    if (!result.supported) {
      markUnsupported();
      return;
    }
    state.supported = true;
    state.lastApplyFailure = result.lastApplyFailure ?? '';
    if (result.restartingTo) {
      // A restart handed off before this page loaded: the process is about
      // to be replaced, so nothing may be offered again.
      state.latestVersion = result.restartingTo;
      state.phase = 'restarting';
      return;
    }
    if (result.restartWaitingFor) {
      // A restart this page asked for before a reload is still waiting.
      state.phase = 'waiting';
      state.waitingFor = result.restartWaitingFor;
      return;
    }
    if (result.checkError) {
      // The backend carries a failed release check as result state rather
      // than an RPC error, so lastApplyFailure above still arrives — a boot
      // notice must not vanish behind an offline check. Rendered exactly like
      // a thrown failure.
      //
      // A failed check also drops the release the backend had resolved, so a
      // Download offered from 'available' could no longer run. That is the one
      // background failure that shows.
      if (background && startedIn !== 'available') return;
      fail(userFacingError(result.checkError, checkFailedMessage), true);
      return;
    }
    state.error = '';
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
    if (stale()) return;
    if (isScopeRefusal(err) || isMethodUnavailableError(err)) {
      // A remote session. The updater RPCs carry `//ao:scope host`, which no
      // session may be granted, so the transport refuses them with
      // `scope_required` naming `host`. That is not a failure worth alarming
      // the user about; it is this session telling us in-app updates aren't
      // reachable from here, which is exactly what the unsupported copy says.
      //
      // `method_not_found` is still accepted beside it: a backend older than
      // this bundle refused the same call by NAME, and a tab that outlived an
      // update must not start reporting an error where it used to say
      // "unsupported".
      markUnsupported();
      return;
    }
    // The call never reached the backend, so its resolved release stands and
    // the connection banner owns the failure.
    if (background && isPassiveConnectionFailure(err)) return;
    fail(userFacingError(err, checkFailedMessage), true);
  }
}

/**
 * markUnsupported puts the store in the "self-update isn't available on this
 * session" resting state. Two paths reach it — the backend answering
 * supported=false (dev build, no updater configured) and a remote session
 * whose `host`-scoped updater RPCs are refused — and neither writes these
 * fields directly, so the two can't drift into leaving stale release metadata
 * behind.
 */
function markUnsupported(): void {
  state.supported = false;
  state.phase = 'idle';
  state.latestVersion = '';
  state.releaseName = '';
  state.releaseNotes = '';
  state.lastApplyFailure = '';
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
    // A background check may be asking right now, and the backend runs this
    // download after it, on whatever release that check resolves. Wait for
    // its answer so the download starts from the offer the page then shows,
    // or not at all.
    await newestCheck;
    if (state.phase !== 'available') return;
  } else if (isUpdateFlowBusy(state.phase)) {
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
    fail(userFacingError(err, 'Could not start the update download.'));
  }
}

/**
 * restartForUpdate swaps in the staged update and relaunches. This quits the
 * running app, so it is only ever wired to an explicit button.
 */
export async function restartForUpdate(): Promise<void> {
  if (state.phase !== 'ready') return;
  // Latch out of 'ready' before the RPC so the button cannot double-fire; see
  // the 'restarting' phase comment for why the process may outlive this call.
  state.phase = 'restarting';
  state.cancelError = '';
  try {
    await RestartToUpdate();
    // Desktop: the app is shutting down and the swap helper relaunches the new
    // version. WSL: the Windows launcher has the directive and this process
    // stays 'restarting' until the launcher kills it — or the backend reports
    // the handoff failed via updater:error, which lands us in 'error'. A
    // busy host answers at once and holds the restart; updater:restart then
    // moves us to 'waiting'.
  } catch (err) {
    fail(userFacingError(err, restartFailedMessage));
  }
}

/**
 * cancelRestartForUpdate ends a restart that is waiting for running work.
 * The update stays downloaded, so the phase returns to 'ready'.
 */
export async function cancelRestartForUpdate(): Promise<void> {
  if (state.phase !== 'waiting' || state.canceling) return;
  state.canceling = true;
  state.cancelError = '';
  try {
    await CancelRestartToUpdate();
    if (state.phase === 'waiting') {
      state.phase = 'ready';
      state.waitingFor = '';
    }
  } catch (err) {
    state.cancelError = userFacingError(err, 'Could not cancel the update.');
  } finally {
    state.canceling = false;
  }
}

let initialized = false;

/**
 * initUpdates subscribes to the backend updater event channels, runs the
 * launch check and starts the repeating background check. Idempotent; returns
 * a cleanup function that tears down the subscriptions and the timer. Call
 * once from the app root.
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
    wailsEventOn<RestartPayload>('updater:restart', (e) => {
      switch (e?.phase) {
        case 'waiting':
          state.phase = 'waiting';
          state.waitingFor = e.waitingFor ?? '';
          return;
        case 'restarting':
          state.phase = 'restarting';
          break;
        case 'canceled':
          if (state.phase === 'waiting' || state.phase === 'restarting') state.phase = 'ready';
          break;
        case 'failed':
          fail(userFacingError(e.error, restartFailedMessage));
          break;
        // 'idle': the launcher abandoned the handoff; updater:error says why.
      }
      state.waitingFor = '';
    }),
    wailsEventOn<ErrorPayload>('updater:error', (e) => {
      const stage = e?.stage ? `${e.stage} ` : '';
      fail(e?.message ? `Update ${stage}failed: ${e.message}` : 'Update failed.');
    }),
  ];

  // Launch check — surfaces availability without installing anything.
  void runUpdateCheck();
  const backgroundChecks = setInterval(() => void startCheck(true), UPDATE_CHECK_INTERVAL_MS);

  return () => {
    clearInterval(backgroundChecks);
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
  state.waitingFor = '';
  state.canceling = false;
  state.cancelError = '';
  state.lastApplyFailure = '';
  state.availableVersions = [];
  state.versionsLoaded = false;
  state.versionsLoading = false;
  state.versionsError = '';
  state.selectedTag = '';
  errorFromCheck = false;
  checkSeq += 1;
  newestCheck = Promise.resolve();
  initialized = false;
}
