// The contract for what runs between tests in the happy-dom and browser
// projects. testResetsPlugin.ts registers each module when it loads, and
// resetRegistry.ts runs what this file says for the modules a test file
// loaded, so a test file pays only for the modules it loads.
//
// Resets by name: after every test, each export named `reset…ForTest` or
// `clear…ForTest` (with or without a leading `__`) of a loaded module
// runs, when the transform sees that it takes no required argument.
//
// The contract of such an export: it returns its own module to the state
// the module had just after it loaded, and it breaks no other module's
// registration (a listener, a step or a source another module installed,
// at its load or when it made something that outlives these steps), and
// it calls no backend. It resets its own module only; another module's
// state is that module's own reset's, and the transform fails a reset
// that calls another module's reset export. An export that cannot keep the
// contract is listed in NOT_BETWEEN_TESTS with the reason. An
// export that requires an argument is never called by name; a module opts
// it in by naming it in POSTURE or FIRST below with a `run` that passes the
// argument.
//
// Order after each test: FIRST in its order, then every other reset in
// module load order (a module's imports load, and reset, before it). FIRST
// stops what calls the mocks or writes storage, resets the mocks and wipes
// storage, so a reset that re-arms against the runtime or binding mocks
// (event listeners, default answers) or reads storage sees the next test's.
//
// Limit: a module with state and no reset export keeps that state across
// the tests of one file. About 169 such modules exist; they are not forced
// to add one. Every module is evaluated afresh for each test file, so no
// state crosses files either way.
//
// Modules are named by path from frontend/, or from node_modules/ for a
// package. This file is read by the transform as well as by the registry,
// so it imports types only.
import type * as TransportStatus from '../../lib/stores/transportStatus.svelte.ts';
import type * as PageHost from '../../lib/transport/pageHost.ts';
import type * as Scopes from '../../lib/transport/scopes.ts';

export const RESET_REGISTRY_KEY = 'agent-overflow.test.resetRegistry';

/** The shape a reset export must have to run by name. */
export const RESET_NAME = /^(?:__)?(?:reset|clear)\w*ForTest$/;

/** The between-test step that wipes localStorage and sessionStorage. */
export const STORAGE = 'storage' as const;

export type Exports = Record<string, unknown>;

export type Step = {
  readonly module: string;
  readonly name: string;
  /** Calls the export, for one that takes arguments. Without it the export
   * is called with none. It receives that export only. */
  readonly run?: (fn: never) => unknown;
};

function step(module: string, name: string): Step;
function step<M, K extends keyof M & string>(module: string, name: K, run: (fn: M[K]) => unknown): Step;
function step(module: string, name: string, run?: (fn: never) => unknown): Step {
  return { module, name, run };
}

/** Run before every test, for the modules loaded by then: the state the app
 * is in at boot that the unit suite has no boot to reach. A module first
 * loaded during a test gets its steps as it loads. A step that reads a
 * module of its import cycle still loading (scopes.ts reads
 * deviceSession.ts) runs when the next module loads, and fails the test if
 * none does. */
export const POSTURE: readonly Step[] = [
  // App starts the sidebar rows' continuity at boot; so does every test.
  step('src/lib/stores/sidebarEntryContinuity.svelte.ts', 'startSidebarEntryContinuity'),
  // With no transport the status snapshot is `disconnected`, which suspends
  // every connection-gated store. A test that cares about an outage drives it.
  step<typeof TransportStatus, '__setTransportStatusForTest'>('src/lib/stores/transportStatus.svelte.ts', '__setTransportStatusForTest', (set) =>
    set({ status: 'connected', nextAttemptAt: null })),
  // With no bootstrap fetch nothing resolves the page's grants, and scopes.ts
  // throws in test mode on a read before they resolve. Pin the embedded
  // webview's answer: the owner's own screen, every scope.
  step<typeof PageHost, 'setHostWindowFromBootstrap'>('src/lib/transport/pageHost.ts', 'setHostWindowFromBootstrap', (set) => set(true)),
  step<typeof Scopes, 'setPageGrantsFromBootstrap'>('src/lib/transport/scopes.ts', 'setPageGrantsFromBootstrap', (set) => set(false)),
  // Git status re-sources whatever is attached when the transport pin
  // above lands, so its reset follows the pin.
  step('src/lib/stores/gitStatusStore.svelte.ts', '__resetGitStatusStoreForTest'),
];

/** Run first after every test, in this order. */
export const FIRST: readonly (Step | typeof STORAGE)[] = [
  step('src/lib/stores/diffReviewComments.svelte.ts', 'resetDiffReviewCommentsForTest'),
  // Unmount before any store resets, so no component reacts to them.
  step('@testing-library/svelte/src/pure.js', 'cleanup'),
  step('src/test/helpers/buildVariant.ts', 'resetBuildVariantPage'),
  // Clear every pane the test created, as closing it does: a pane's replica
  // write-back fires 2s after its sync, into the next test's replica.
  step('src/test/openThreadPanes.ts', 'clearOpenThreadPanes'),
  // Panes hold timers that call the binding mocks; clear them while the
  // mocks are still installed.
  step('src/lib/stores/panes.svelte.ts', 'resetPanesForTest'),
  step('src/test/helpers/backends.ts', 'resetStagedBackends'),
  step('src/test/mocks/wailsio-runtime.ts', 'resetWailsMocks'),
  step('src/test/mocks/bindings-app.ts', 'resetBindingMocks'),
  step('src/test/mocks/attachmentTransfer.ts', 'resetAttachmentTransferMocks'),
  // The size priors flush on a debounce; stop it before the wipe so it
  // cannot write into the next test's storage.
  step('src/lib/utils/virtual/priors.ts', 'clearAllThreadSizePriorsForTest'),
  step('src/lib/utils/virtual/priorsStorage.ts', '__resetSizePriorsStorageForTest'),
  STORAGE,
  // appStorage's in-memory buckets outlive the storage wipe.
  step('src/lib/stores/appStorage.ts', 'reinitAppStorageForTest'),
];

/** Reset-named exports that do not run by name, each with its reason. */
export const NOT_BETWEEN_TESTS: Readonly<Record<string, string>> = {
  'src/lib/stores/paneLayout.svelte.ts#resetPaneLayoutForTest': 'sets the default main pane, which the module does not hold when it loads',
  'src/lib/stores/transportStatus.svelte.ts#resetTransportStatusForTest': 'tears down the backend status subscriptions the module makes when it loads, for a test that seeds a fresh client',
  'src/lib/stores/watchedThreads.ts#resetWatchedThreadSourcesForTest': 'drops the watch sources other modules register when they load, which they do not register again',
  'src/lib/transport/clientPurge.ts#__resetClientPurgeForTest': 'drops the purge steps other modules register when they load, which they do not register again',
  'src/lib/transport/backendClock.ts#resetBackendClocksForTest': 'drops the clock each backend entry registers when it is made; the home entry outlives the between-test steps and does not register again',
};

function byModule(ids: Iterable<readonly [string, string]>): Map<string, Set<string>> {
  const named = new Map<string, Set<string>>();
  for (const [module, name] of ids) {
    const names = named.get(module) ?? new Set<string>();
    names.add(name);
    named.set(module, names);
  }
  return named;
}

/** Every module POSTURE and FIRST name, with the exports they call. */
export function stepExports(): Map<string, Set<string>> {
  return byModule([...POSTURE, ...FIRST].flatMap((entry) => (entry === STORAGE ? [] : [[entry.module, entry.name] as const])));
}

/** Every module this file names, with the exports it names: the step
 * exports and those that do not run between tests. None runs by name. */
export function namedExports(): Map<string, Set<string>> {
  return byModule([
    ...[...stepExports()].flatMap(([module, names]) => [...names].map((name) => [module, name] as const)),
    ...Object.keys(NOT_BETWEEN_TESTS).map((id) => id.split('#') as [string, string]),
  ]);
}
