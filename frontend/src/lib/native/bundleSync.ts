// The phone shell's update channel: the backend it is paired with is
// also its app store (docs/specs/remote-access.md §9, "Bundle sync").
//
// A shell runs the bundle its APK was built with until an attached
// backend says it serves a different one. Then this module downloads
// that bundle over the paired session, hands it to the native plugin to
// verify and stage, and says one sentence about a restart. The swap
// happens on the next cold start; the native side rolls back if the
// first boot on it never reports healthy (mobile/AGENTS.md § The bundle
// plugin).
//
// **Never blocking, never urgent.** Nothing here delays a connection, an
// approval or a render. Every failure is a log line and a later retry,
// because a phone that keeps running the bundle it already has is
// working perfectly.
//
// **Which backend, when several are attached.** The rule is "run the
// newest attached backend's bundle": the highest `bundleVersion` among
// the backends that publish one, home on ties and whenever the versions
// do not parse. One app cannot run two bundles, and picking the newest
// is the only choice that converges — picking home would strand a phone
// on an old desktop, and picking "the most recently attached" would make
// the answer depend on the order somebody paired.
//
// **The decision is a pure function** (`decideBundleSync`), one row per
// case, each row a unit test. What is left around it is the plumbing:
// subscribe, fetch, stage, re-read.
//
// **What the person sees**: nothing, until a bundle is staged. Then the
// one sentence in `stores/bundleNotice.svelte.ts`.

import { onBackendHelloChange } from '../stores/transportStatus.svelte';
import { noteBundleReady, noteBundleTooOld } from '../stores/bundleNotice.svelte';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { pairedSessionHeaders } from '../transport/deviceSession';
import type { LeaseState } from '../transport/frames';
import { backendCredentials, backendUrl } from '../transport/homeEndpoint';
import { clientLease, onClientLeaseChange } from '../transport/lease';
import {
  RECONNECT_INITIAL_MS,
  RECONNECT_MAX_REMOTE_MS,
  type TransportHello,
} from '../transport/wsClient';
import { isNativeShell } from './platform';
import {
  bundlePlugin,
  type BundleManifest,
  type BundleState,
  type BundleSyncPlugin,
} from './plugins';

/** The routes `internal/transport/bundleroutes.go` serves. */
const MANIFEST_PATH = '/bundle/manifest.json';
const ARCHIVE_PATH = '/bundle/archive.zip';

/**
 * Where a built bundle records its own id, relative to the page.
 *
 * Written into `dist` by `frontend/scripts/bundleId.ts` at build time
 * with the same rule `internal/bundle` computes. It answers the one
 * question the native state file cannot: what the APK's OWN assets are,
 * for a phone that has never staged anything. Without it every such
 * phone would download the bundle it is already running, once per
 * connection, forever.
 */
const APK_BUNDLE_ID_PATH = 'bundle-id.txt';

/**
 * How many consecutive failures one bundle id is allowed before this
 * launch stops asking for it.
 *
 * The delay is capped by the transport's own ladder; this caps the
 * ATTEMPTS, because the failure this guards against is a backend that
 * will never serve a bundle this phone can take, and retrying that on a
 * metered link until the app is killed is a data bill rather than a
 * recovery. A different id resets it, and so does a relaunch.
 */
export const MAX_ATTEMPTS_PER_BUNDLE = 6;

/** A backend that publishes a bundle, flattened out of its hello. */
export interface BundleCandidate {
  backend: BackendKey;
  backendName: string;
  bundleId: string;
  bundleVersion: string;
  minShellBuild: number;
}

/** Everything `decideBundleSync` reads. No I/O, no clock. */
export interface BundleSyncInput {
  /** The backend whose bundle this shell should be running, or null. */
  target: BundleCandidate | null;
  /** The id this shell is executing right now. '' when it cannot say. */
  running: string;
  /** The native store's answer, as `state()` last returned it. */
  state: BundleState;
  /** Whether the OS has this app in the foreground. */
  lease: LeaseState;
  /** The id being fetched right now, or '' when nothing is. */
  inFlight: string;
  /** How many times THIS launch has already tried to install `target`. */
  attempts: number;
}

/** What to do about `target`, and why. */
export type BundleDecision =
  /** Nothing to do: no bundle offered, or the offered one is already here. */
  | { kind: 'idle' }
  /** Already downloaded once and failed its first boot. Never again. */
  | { kind: 'rolled-back'; id: string }
  /** Tried and failed too many times this launch. Wait for a relaunch. */
  | { kind: 'exhausted'; id: string }
  /** This APK is below the bundle's floor. Say so; download nothing. */
  | { kind: 'too-old'; id: string; backendName: string }
  /** The app is paused. Wait for the foreground. */
  | { kind: 'deferred'; id: string }
  /** This exact id is already downloading. Join it. */
  | { kind: 'joined'; id: string }
  /** A different id is downloading. Reconsider when it settles. */
  | { kind: 'busy'; id: string }
  /** Fetch it. */
  | { kind: 'download'; id: string; backend: BackendKey };

/**
 * One decision, from facts alone.
 *
 * The order of the rows is the policy. Cheapest and most final first:
 * a bundle that is already running or already staged ends it, a bundle
 * that failed before is refused before anything is compared, and the
 * version floor is answered before a byte is fetched.
 */
export function decideBundleSync(input: BundleSyncInput): BundleDecision {
  const target = input.target;
  if (target === null || target.bundleId === '') return { kind: 'idle' };
  const id = target.bundleId;

  // Already the running bundle, or already staged for the next start.
  // The staged case is what keeps a phone from re-downloading between
  // the moment it stages and the moment somebody restarts the app.
  if (id === input.running || id === input.state.next) return { kind: 'idle' };

  if (input.state.rolledBack.includes(id)) return { kind: 'rolled-back', id };

  // `versionCode` is 0 when the platform could not answer, which is
  // below every floor a bundle can state — the safe direction, since a
  // phone that cannot say what it is should not take a bundle it may not
  // be able to run.
  if (target.minShellBuild > input.state.versionCode) {
    return { kind: 'too-old', id, backendName: target.backendName };
  }

  if (input.lease === 'background') return { kind: 'deferred', id };

  if (input.inFlight === id) return { kind: 'joined', id };
  if (input.inFlight !== '') return { kind: 'busy', id };

  // The cap lives HERE, in the decision, and not beside the retry timer.
  // A failed attempt has to answer the very next hello as well as its own
  // retry, and a cap the decision could not see was a cap that only slowed
  // the schedule down: `run()` re-evaluated after every failure and this
  // row returned `download` again, so a backend serving a bundle this
  // phone cannot take was refetched, archive and all, with no delay and no
  // end. A different id resets the count, and so does a relaunch.
  if (input.attempts >= MAX_ATTEMPTS_PER_BUNDLE) return { kind: 'exhausted', id };

  return { kind: 'download', id, backend: target.backend };
}

/**
 * The newest bundle among the attached backends.
 *
 * Highest `bundleVersion` wins. A version that does not parse ranks
 * below every one that does, and home wins every tie — including the tie
 * where nothing parses at all, which is what a fleet of `dev` builds
 * looks like.
 */
export function pickBundleSource(candidates: readonly BundleCandidate[]): BundleCandidate | null {
  let best: BundleCandidate | null = null;
  let bestParts: readonly number[] | null = null;
  for (const candidate of candidates) {
    if (candidate.bundleId === '') continue;
    const parts = parseVersion(candidate.bundleVersion);
    if (best === null) {
      best = candidate;
      bestParts = parts;
      continue;
    }
    const order = compareVersions(parts, bestParts);
    if (order > 0 || (order === 0 && candidate.backend === HOME_BACKEND)) {
      best = candidate;
      bestParts = parts;
    }
  }
  return best;
}

/**
 * `major.minor.patch` out of a version string, or null.
 *
 * Deliberately small: a leading `v` is tolerated because tags carry one,
 * a pre-release or build suffix is ignored because it cannot order two
 * bundles more usefully than the numbers already did, and anything else
 * — `dev` above all — is "does not parse", which the caller reads as
 * "rank below anything that does".
 */
function parseVersion(version: string): readonly number[] | null {
  const match = /^v?(\d+)\.(\d+)(?:\.(\d+))?/.exec(version.trim());
  if (match === null) return null;
  return [Number(match[1]), Number(match[2]), Number(match[3] ?? 0)];
}

function compareVersions(a: readonly number[] | null, b: readonly number[] | null): number {
  if (a === null && b === null) return 0;
  if (a === null) return -1;
  if (b === null) return 1;
  for (let i = 0; i < 3; i++) {
    const diff = (a[i] ?? 0) - (b[i] ?? 0);
    if (diff !== 0) return diff > 0 ? 1 : -1;
  }
  return 0;
}

/** One backend's hello, as a candidate. Null when it publishes no bundle. */
export function candidateFrom(
  backend: BackendKey,
  hello: TransportHello | null,
): BundleCandidate | null {
  if (hello === null || hello.bundleId === '') return null;
  return {
    backend,
    backendName: hello.backendName,
    bundleId: hello.bundleId,
    bundleVersion: hello.bundleVersion,
    minShellBuild: hello.minShellBuild,
  };
}

// ---------------------------------------------------------------------------
// The door
// ---------------------------------------------------------------------------

/** What one attached backend's hello left behind. */
const candidates = new Map<BackendKey, BundleCandidate>();

let plugin: BundleSyncPlugin | null = null;
let installed: (() => void)[] = [];
let nativeState: BundleState | null = null;
let runningId = '';
let inFlight = '';
let attemptsById = new Map<string, number>();
let retryTimer: ReturnType<typeof setTimeout> | null = null;
let toldTooOld = '';
let toldExhausted = '';
let readyReported = false;

/**
 * Start watching every attached backend's hello, on a shell only.
 *
 * Answers a teardown. Called from `native/boot.ts` once, after the app
 * has mounted: nothing here is on the boot path, and a phone that never
 * gets this far is a phone that could not have downloaded anything
 * anyway.
 */
export async function startBundleSync(): Promise<() => void> {
  if (!isNativeShell()) return () => {};
  plugin = await bundlePlugin();
  // No plugin means an APK built before this seam existed. It keeps
  // running its own bundle, which is exactly right.
  if (plugin === null) return () => {};
  try {
    nativeState = await plugin.state();
  } catch (err) {
    console.warn('bundleSync: the native store did not answer', err);
    return () => {};
  }
  runningId = nativeState.current !== '' ? nativeState.current : await readApkBundleId();

  installed = [
    onBackendHelloChange((backend, hello) => {
      const candidate = candidateFrom(backend, hello);
      if (candidate === null) candidates.delete(backend);
      else candidates.set(backend, candidate);
      evaluate();
    }),
    onClientLeaseChange(() => evaluate()),
  ];
  return stopBundleSync;
}

/** Drop every subscription and forget this launch's progress. */
export function stopBundleSync(): void {
  for (const cancel of installed) cancel();
  installed = [];
  if (retryTimer !== null) {
    clearTimeout(retryTimer);
    retryTimer = null;
  }
  candidates.clear();
  attemptsById = new Map();
  plugin = null;
  nativeState = null;
  runningId = '';
  inFlight = '';
  toldTooOld = '';
  toldExhausted = '';
  readyReported = false;
}

/**
 * Confirm this launch healthy, once.
 *
 * Called by `native/boot.ts` after the app has mounted. Reaching this
 * call IS the health check: the bundle's module graph loaded, `main.ts`
 * ran to the end, the app rendered, and the plugin the bundle's native
 * seams depend on answered. A bundle that fails any of those never gets
 * here, and the native watchdog rolls it back (mobile/AGENTS.md § The
 * bundle plugin).
 *
 * Deliberately NOT "reached hello". A phone launched with no network
 * would roll back a working bundle, record its id as failed, and refuse
 * it on every later hello — stranded on the old app until the desktop
 * built a newer one. A bundle that boots but cannot reach its backend
 * shows the transport banner, which is that problem's own surface.
 *
 * Idempotent per launch and never throws: a phone whose plugin refuses
 * this is one that will roll back on its own, which is the safe end.
 */
export async function reportBundleHealthy(): Promise<void> {
  if (readyReported || !isNativeShell()) return;
  readyReported = true;
  try {
    const bridge = plugin ?? (await bundlePlugin());
    if (bridge === null) return;
    await bridge.ready();
    // The store just pruned; re-read so a later decision is made against
    // what is on disk rather than what was there at boot.
    nativeState = await bridge.state();
  } catch (err) {
    console.warn('bundleSync: this launch could not be confirmed healthy', err);
  }
}

/**
 * The health check as `boot.ts` spells it. Never rejects: a bundle that
 * cannot confirm is one the native watchdog rolls back 30 seconds into
 * the launch.
 */
export async function confirmLaunchHealthy(): Promise<void> {
  if (!isNativeShell()) return;
  await reportBundleHealthy();
}

function evaluate(): void {
  if (plugin === null || nativeState === null) return;
  const target = pickBundleSource([...candidates.values()]);
  const decision = decideBundleSync({
    target,
    running: runningId,
    state: nativeState,
    lease: clientLease(),
    inFlight,
    attempts: target === null ? 0 : (attemptsById.get(target.bundleId) ?? 0),
  });
  switch (decision.kind) {
    case 'idle':
    case 'joined':
    case 'busy':
    case 'deferred':
      return;
    case 'exhausted':
      // Said once per id: `toldExhausted` is the same shape `toldTooOld`
      // has, and for the same reason. Nothing more happens for this id
      // until the app is launched again.
      if (toldExhausted === decision.id) return;
      toldExhausted = decision.id;
      console.warn(
        `bundleSync: ${decision.id} failed ${MAX_ATTEMPTS_PER_BUNDLE} times this launch; not fetching it again`,
      );
      return;
    case 'rolled-back':
      // Once per launch per id would need a second set to remember; the
      // native store already remembers, and this only fires on a hello
      // edge, so it is a line per reconnect at worst.
      console.info(`bundleSync: ${decision.id} already failed on this phone; not fetching it`);
      return;
    case 'too-old':
      if (toldTooOld === decision.id) return;
      toldTooOld = decision.id;
      noteBundleTooOld(decision.backendName);
      return;
    case 'download':
      void run(decision.id, decision.backend);
      return;
  }
}

async function run(id: string, backend: BackendKey): Promise<void> {
  inFlight = id;
  const attempt = (attemptsById.get(id) ?? 0) + 1;
  attemptsById.set(id, attempt);
  // Named apart from the module-level `installed` listener list on purpose:
  // one tail call reads it and it means "this attempt staged a bundle".
  let staged = false;
  try {
    await fetchAndStage(id, backend);
    attemptsById.delete(id);
    // Read back rather than assume: the store decides what `next` is,
    // and a decision made against a guess would re-download on the very
    // next hello if it guessed wrong.
    nativeState = await plugin!.state();
    noteBundleReady();
    staged = true;
  } catch (err) {
    console.warn(`bundleSync: ${id} did not install (attempt ${attempt})`, err);
    if (attempt < MAX_ATTEMPTS_PER_BUNDLE) scheduleRetry(attempt);
  } finally {
    inFlight = '';
  }
  // Only a SUCCESS re-evaluates here. After a failure the retry timer owns
  // the next look, and it is the only thing that may: re-evaluating inline
  // put the very next attempt on the same tick as the one that just failed,
  // which is a download loop with no delay in it. A hello that arrived
  // mid-download evaluates on its own edge either way.
  if (staged) evaluate();
}

/**
 * The transport's backoff shape, reused rather than re-invented: the
 * same ladder, the same remote cap, the same full jitter with a floor
 * (transport/wsClient.ts). A retry storm from a phone is the one thing a
 * backend on a home connection should never have to absorb.
 */
function scheduleRetry(attempt: number): void {
  if (retryTimer !== null) clearTimeout(retryTimer);
  const base = Math.min(RECONNECT_INITIAL_MS * 2 ** attempt, RECONNECT_MAX_REMOTE_MS);
  const delay = Math.max(50, Math.floor(Math.random() * base));
  retryTimer = setTimeout(() => {
    retryTimer = null;
    evaluate();
  }, delay);
}

/**
 * Manifest, archive, stage.
 *
 * The manifest is re-checked against the id the hello named. A mismatch
 * is an ordinary race — the backend rebuilt between the frame and the
 * fetch — so it throws, and the hello that lands after the rebuild
 * starts the sequence again with the new id.
 */
async function fetchAndStage(id: string, backend: BackendKey): Promise<void> {
  const manifest = await fetchJSON<BundleManifest>(MANIFEST_PATH, backend);
  if (manifest.id !== id) {
    throw new Error(`the backend now serves ${manifest.id}, not ${id}`);
  }
  const archive = await fetchBytes(ARCHIVE_PATH, backend);
  await plugin!.stage({ id, manifest, archiveBase64: base64(archive) });
}

/**
 * One paired request to one attached backend.
 *
 * The session credential and the device proof, the same pair
 * `/bootstrap.json` presents — the bundle routes accept nothing less
 * (internal/transport/bundleroutes.go), because their consumer is a page
 * on an origin the backend never served. The proof is bound to the
 * METHOD and PATH, so each route mints its own.
 */
async function pairedFetch(path: string, backend: BackendKey): Promise<Response> {
  const headers = await pairedSessionHeaders('GET', path, backend);
  const response = await fetch(backendUrl(path, backend), {
    headers,
    credentials: backendCredentials(backend),
  });
  if (!response.ok) {
    throw new Error(`${path} answered ${response.status}`);
  }
  return response;
}

async function fetchJSON<T>(path: string, backend: BackendKey): Promise<T> {
  return (await (await pairedFetch(path, backend)).json()) as T;
}

async function fetchBytes(path: string, backend: BackendKey): Promise<ArrayBuffer> {
  return await (await pairedFetch(path, backend)).arrayBuffer();
}

/**
 * The id the APK's own assets were built with, or '' when the file is
 * not there.
 *
 * Read from the PAGE, not from a backend: it describes what this shell
 * is running. Only ever read when the native store says `current` is
 * empty, which is the one case where the running bundle came from the
 * APK — a staged bundle carries no such file, since the id is computed
 * over the tree and cannot be inside it.
 */
async function readApkBundleId(): Promise<string> {
  try {
    const response = await fetch(APK_BUNDLE_ID_PATH);
    if (!response.ok) return '';
    return (await response.text()).trim();
  } catch (err) {
    console.warn('bundleSync: this build does not say what bundle it is', err);
    return '';
  }
}

/**
 * Bytes to base64, in chunks.
 *
 * The Capacitor bridge marshals JSON, so an archive crosses it as text
 * once per update — a few MB, on the microtask after a download that
 * already took longer. Chunked because `String.fromCharCode(...bytes)`
 * on a multi-megabyte array is a stack overflow rather than a slow path.
 */
function base64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  const CHUNK = 0x8000;
  let binary = '';
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}
