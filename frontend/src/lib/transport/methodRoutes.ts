// HAND-WRITTEN PLACEHOLDER. `methodgen` REPLACES THIS FILE.
//
// Wave 7a of the remote-access campaign grows the Go method table a Route
// column (docs/specs/remote-access.md §10, "Routing an RPC") and emits this
// module from it, exactly as it already emits the scope table. Until that
// generator lands, this file states the same shape by hand so the TS side
// of the routing seam can be written and tested against it. At merge the
// generator's output takes this path verbatim and nothing above it moves.
//
// What the generator emits, and what this file therefore declares:
//
//   export type MethodRoute = 'thread' | 'project' | 'home' | 'selected' | 'all';
//   export const METHOD_ROUTES: Readonly<Record<number, MethodRoute>>;
//
// keyed by the numeric Wails method id — the same id the generated
// bindings pass to `Call.ByID`. The table below is DELIBERATELY PARTIAL:
// it carries only the methods the route-resolution tests exercise, because
// a hand-maintained copy of 371 rows would be a second source of truth
// that drifts. An id with no row resolves `home`, which is exactly today's
// single-connection behaviour, so a partial table is a working app rather
// than a broken one (./runtime.ts states that fallback and warns once per
// method in dev).
//
// `methodRoutes.test.ts` pins every id below against the `Call.ByID(<n>`
// site in `frontend/bindings/agent-overflow/app.ts`, so a regeneration of
// the bindings that moves an id fails here rather than silently routing a
// call to the wrong machine.

/**
 * Where a call goes, decided by the method rather than by the caller.
 *
 * - `thread`: argument 0 is a thread id; the call goes to the backend that
 *   owns that thread (./entityIndex.ts).
 * - `project`: argument 0 is a project id; same, for projects.
 * - `home`: the page's own backend — host actions, this machine's
 *   settings, this backend's access administration.
 * - `selected`: creation-shaped calls, which have no entity to resolve
 *   from yet; the composer's chosen backend answers
 *   (`stores/selectedBackend.svelte.ts`).
 * - `all`: fan out to every attached backend and merge the answers
 *   (./backends.ts `mergeBackendResults` owns the rule).
 */
export type MethodRoute = 'thread' | 'project' | 'home' | 'selected' | 'all';

export const METHOD_ROUTES: Readonly<Record<number, MethodRoute>> = {
  // The two list calls that feed the unified sidebar.
  1090132042: 'all', // ListThreads
  2721360259: 'all', // ListProjects
  // Thread-keyed reads and writes: argument 0 is the thread id.
  1098302047: 'thread', // GetThread
  1186337974: 'thread', // DeleteThread
  1496882310: 'thread', // SendMessage
  2604956482: 'thread', // ListRecentThreadItems
  3841902986: 'thread', // SyncThreadWindow
  3282404643: 'thread', // GitStatusSubscribe
  4123560639: 'thread', // GetGitStatus
  // Project-keyed: argument 0 is the project id.
  471350242: 'project', // GetProjectWorktreeSetup
  409101231: 'project', // GitListWorktreesForProject
  2575010484: 'project', // ProjectDeletionPreview
  // Creation-shaped: nothing to resolve from, so the composer decides.
  // `CreateProject` is one of these and not `project`-routed: its first
  // argument is a filesystem PATH, and the project id it mints does not
  // exist until the call answers.
  2579322833: 'selected', // CreateThread
  969543070: 'selected', // CreateProject
  // The page's own backend.
  3380106838: 'home', // GetUIState
  1514250938: 'home', // SetUIState
  1186757769: 'home', // DeleteUIState
  2554697378: 'home', // GetSettings
  981125684: 'home', // ListProviderAccounts
  3214812657: 'home', // BeginPasskeyStepUp
  1569276637: 'home', // FinishPasskeyStepUp
};
