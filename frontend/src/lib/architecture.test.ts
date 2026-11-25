// Structural enforcement for the state-ownership doctrine in
// frontend/CLAUDE.md → "State Boundaries". These rules are the part of that
// doctrine a reviewer would otherwise have to remember: they read the tree, so
// a regression fails `pnpm test` instead of surviving to a bug report.
//
// Both rules use the same allowlist mechanic and it is SHRINK-ONLY: a new
// offender fails, and so does an allowlist entry that no longer offends. An
// exception that has been fixed must be deleted, or the list stops describing
// the tree and starts hiding it.
//
// The walk, the exclusion set and the allowlist comparison are shared with
// `themeTokens.test.ts` through `test/sourceScan.ts`.

import { dirname, join, resolve, sep } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  SRC_ROOT,
  expectAllowlistExact,
  repoPath,
  scannedSources,
} from '../test/sourceScan';
import browserCompanionActions from '../../../internal/app/app_browser_companion_action.go?raw';
import { findCompositorSourceFindings } from '../test/compositorSourceScan';
import { findContextMenuPropagationFindings } from '../test/contextMenuSourceScan';
import { NUMBERED_KINDS } from './stores/browserActionWire';

// What this suite scans (test/sourceScan.ts).
const SCANNED = scannedSources(import.meta.glob<string>(
  ['/src/**/*.{ts,svelte,css}', '!/src/test/**', '!**/*.{test,spec}.{ts,js}', '!**/*.manual.ts'],
  { query: '?raw', import: 'default', eager: true },
));

const STORES_DIR = join(SRC_ROOT, 'lib', 'stores');
const BINDINGS_MODULE = join(STORES_DIR, 'bindings');
// The Wails-generated tree, one level up from src/. Importing an RPC straight
// out of here bypasses `stores/bindings.ts` entirely.
const GENERATED_BINDINGS_DIR = resolve(SRC_ROOT, '..', 'bindings');

// ---------------------------------------------------------------------------
// Rule 1 — entity-owned RPCs are store-only.
//
// State is keyed by its ENTITY, and the entity store built on
// `stores/entityStore.svelte.ts` is that entity's single write chokepoint: one
// subscription, one observation, every consumer `$derived` from it. A surface
// that calls the underlying RPC itself gets a private copy that nothing else
// can heal — which is how two panes on one worktree disagreed about whether
// there was anything to commit for minutes at a time (audit 2026-08-08).
//
// This is deliberately NOT "components may not import stores/bindings". 87
// non-test component files import bindings today and almost all of them are
// imperative commands (GitCommit, WorkflowStartRun) — calls, not
// state. A blanket rule would need an 87-entry allowlist that a new
// `GetGitStatus` caller would simply be pre-approved by. Naming the RPCs an
// entity store owns is the rule that actually catches the regression.
//
// The rule covers `stores/` too, per OWNER: an entity store may import the
// RPCs it owns and no others. Two stores reaching the same subscribe RPC is
// the same defect one directory in — the second one gets a private copy, and
// "it's already in stores/" is exactly the reasoning that would wave it
// through.
//
// A new entity store adds its RPCs here. That is the registry's whole job.
interface EntityOwnedBinding {
  /** Repo-relative (src/-relative) path of the store that owns the RPC. */
  readonly owner: string;
  /** Where inside that store it is called, for the failure message. */
  readonly where: string;
}

function owned(owner: string, where: string): EntityOwnedBinding {
  return { owner, where };
}

const GIT_STATUS_STORE = 'lib/stores/gitStatusStore.svelte.ts';
const PR_REVIEW_STORE = 'lib/stores/prReviewStore.svelte.ts';
const PR_REVIEW_CONFLICTS = 'lib/stores/prReviewConflicts.svelte.ts';
const PULLS_PAGE_STORE = 'lib/stores/pullsPage.svelte.ts';
const PIPELINE_CI_STORE = 'lib/stores/pipelineCI.svelte.ts';
const PR_DRAFTS_STORE = 'lib/stores/prDiffReviewComments.svelte.ts';
const MCP_SERVERS_STORE = 'lib/stores/mcpServers.svelte.ts';
const CHAT_BAR_FAVORITES_STORE = 'lib/stores/chatBarFavorites.svelte.ts';
const WORKFLOW_RUN_MAP_STORE = 'lib/stores/workflowRunMap.svelte.ts';
const BACKGROUND_TASK_OUTPUT_STORE = 'lib/stores/backgroundTaskOutput.svelte.ts';
const APPEARANCE_STORE = 'lib/stores/appearance.svelte.ts';
const SOUNDS_STORE = 'lib/stores/sounds.svelte.ts';
const EDITORS_STORE = 'lib/stores/editors.svelte.ts';
const BROWSER_COMPANION_STORE = 'lib/stores/browserCompanion.svelte.ts';
const BROWSER_FIND_STORE = 'lib/stores/browserFind.svelte.ts';
const PROVIDER_ACCOUNTS_STORE = 'lib/stores/computerAccounts.svelte.ts';
const SYSTEMS_STORE = 'lib/stores/systems.svelte.ts';
const SERVICE_UPDATE_STORE = 'lib/stores/serviceUpdate.svelte.ts';
const DEV_SERVERS_STORE = 'lib/stores/devServers.svelte.ts';
const PROVIDER_UPDATES_STORE = 'lib/stores/providerUpdates.svelte.ts';
const THREADS_STORE = 'lib/stores/threads.svelte.ts';
const TRACKER_CONNECTIONS_STORE = 'lib/stores/trackerConnections.svelte.ts';
const TRACKER_PROJECT_BOARDS_STORE = 'lib/stores/trackerProjectBoards.svelte.ts';

const ENTITY_OWNED_BINDINGS: Record<string, EntityOwnedBinding> = {
  BrowserCompanionPaneAttach: owned(BROWSER_COMPANION_STORE, 'attachBrowserCompanion()'),
  BrowserCompanionPaneDetach: owned(BROWSER_COMPANION_STORE, 'the attachment release()'),
  BrowserCompanionPaneRect: owned('lib/stores/browserPaneRectReports.ts', 'reportBrowserPaneRect()'),
  BrowserCompanionDo: owned('lib/stores/browserActionWire.ts', 'sendFromPane() / sendForThread()'),
  BrowserCompanionRecentAddresses: owned('lib/stores/browserActionWire.ts', 'readRecentAddresses()'),
  BrowserCompanionRevealPageFile: owned('lib/stores/browserActionWire.ts', 'revealPageFile()'),
  BrowserCompanionThreadState: owned(BROWSER_COMPANION_STORE, 'hydrateBrowserCompanionState()'),
  BrowserCompanionFind: owned(BROWSER_FIND_STORE, 'createBrowserFind()'),
  // The read marker is the one thread-row field where the newest value is
  // not the largest one (explicit unread persists as epoch 0), so the
  // merge in eventsThreadRows.ts can only tell a local write from a stale
  // wire row by asking whether one is in flight. Both RPCs are made under
  // that claim (threadReadWrites.ts) and a caller that made either one
  // directly would produce exactly the row the claim exists to settle.
  MarkThreadRead: owned(THREADS_STORE, 'markThreadRead()'),
  MarkThreadUnread: owned(THREADS_STORE, 'markThreadUnread()'),
  GetGitStatus: owned(GIT_STATUS_STORE, 'refreshGitStatus()'),
  GitStatusSubscribe: owned(GIT_STATUS_STORE, 'attachGitStatus()'),
  GitStatusUnsubscribe: owned(GIT_STATUS_STORE, 'the attachment release()'),
  UpdateThreadBranch: owned(GIT_STATUS_STORE, 'the onApply branch reconciliation'),
  SubscribePRUpdates: owned(PR_REVIEW_STORE, 'attachPR()'),
  UnsubscribePRUpdates: owned(PR_REVIEW_STORE, 'the attachment release()'),
  SetPRUpdatesActive: owned(PR_REVIEW_STORE, 'handlePRVisibilityChange()'),
  GetPRMergeConflicts: owned(PR_REVIEW_CONFLICTS, 'openPRConflicts()'),
  GetMergeConflictFile: owned(PR_REVIEW_CONFLICTS, 'ensurePRConflictFile()'),
  RefreshPRCI: owned(PR_REVIEW_STORE, 'the CI handle refreshPRCI() addresses'),
  SetPRCILogFollows: owned(PR_REVIEW_STORE, 'the CI handle setPRCILogFollow() addresses'),
  GetPRCILogLines: owned('lib/stores/ciLogBlocks.svelte.ts', 'ciLogFetch()'),
  // The Pull requests page: the list pump's handle, a repository
  // pipeline's pump, and the drafts a PR owns.
  SubscribePRList: owned(PULLS_PAGE_STORE, 'attachPullsPage()'),
  UnsubscribePRList: owned(PULLS_PAGE_STORE, 'detachPullsPage()'),
  SetPRListActive: owned(PULLS_PAGE_STORE, 'the visibility votes'),
  RefreshPRList: owned(PULLS_PAGE_STORE, 'refreshPullsPage()'),
  SubscribePipeline: owned(PIPELINE_CI_STORE, 'attachPipelineCI()'),
  UnsubscribePipeline: owned(PIPELINE_CI_STORE, 'the attachment release()'),
  SetPipelineActive: owned(PIPELINE_CI_STORE, 'handlePipelineVisibilityChange()'),
  SetPipelineFollows: owned(PIPELINE_CI_STORE, 'the CI handle setPRCILogFollow() addresses'),
  RefreshPipeline: owned(PIPELINE_CI_STORE, 'the CI handle refreshPRCI() addresses'),
  ListPRDiffReviewComments: owned(PR_DRAFTS_STORE, 'refreshPRDiffReviewComments()'),
  CreatePRDiffReviewComment: owned(PR_DRAFTS_STORE, 'createPRDiffReviewComment()'),
  UpdatePRDiffReviewComment: owned(PR_DRAFTS_STORE, 'updatePRDiffReviewComment()'),
  DeletePRDiffReviewComment: owned(PR_DRAFTS_STORE, 'deletePRDiffReviewComment()'),
  MarkPRDiffReviewCommentsSent: owned(PR_DRAFTS_STORE, 'markPRDiffReviewCommentsSent()'),
  SendPRDiffReviewComments: owned(PR_DRAFTS_STORE, 'sendPRDiffReviewComments()'),
  ListThreadMcpServers: owned(MCP_SERVERS_STORE, 'attachMcpServers()'),
  ListWorkspaceMcpServers: owned(MCP_SERVERS_STORE, 'attachMcpServers()'),
  SetThreadMcpServerEnabled: owned(MCP_SERVERS_STORE, 'setMcpServerEnabled()'),
  SetWorkspaceMcpServerEnabled: owned(MCP_SERVERS_STORE, 'setMcpServerEnabled()'),
  GetMcpServerStatus: owned(MCP_SERVERS_STORE, 'the shared row status on peekMcpServers()'),
  RefreshMcpServerStatus: owned(MCP_SERVERS_STORE, 'refreshMcpServerStatus()'),
  ReconnectMcpServer: owned(MCP_SERVERS_STORE, 'reconnectMcpServer()'),
  TriggerMcpAuth: owned(MCP_SERVERS_STORE, 'triggerMcpAuth()'),
  TriggerWorkspaceMcpAuth: owned(MCP_SERVERS_STORE, 'triggerMcpAuth()'),
  ListChatBarFavorites: owned(
    CHAT_BAR_FAVORITES_STORE,
    'ensureChatBarFavorites() + peekChatBarFavorites()',
  ),
  SetChatBarFavorite: owned(CHAT_BAR_FAVORITES_STORE, 'frontend-local setChatBarFavorite(); backend writes exist only for older clients'),
  WorkflowGetRunMap: owned(WORKFLOW_RUN_MAP_STORE, 'the run-map entity source'),
  WatchBackgroundTaskOutput: owned(BACKGROUND_TASK_OUTPUT_STORE, 'attachBackgroundTaskOutput()'),
  UnwatchBackgroundTaskOutput: owned(BACKGROUND_TASK_OUTPUT_STORE, 'the attachment release()'),
  // Appearance is one app-global value over one directory, so its store is a
  // plain module store rather than an entityStore — but the ownership rule is
  // the same one, and it has teeth here: a settings section that called
  // SetAppearance itself would hold a selection the applier never sees, and a
  // surface that called SetWindowBackgroundColor outside the store would race
  // the cache the next launch reads.
  GetThemeFiles: owned('lib/stores/appearanceFiles.ts', 'readAppearanceFiles() / copyAppearanceFiles()'),
  GetSpinnerFiles: owned('lib/stores/appearanceFiles.ts', 'readSpinnerFiles() / copyAppearanceFiles()'),
  // The cue library is one directory on one host with two mutations that
  // invalidate the listing. A section calling PutSoundFile itself would hold a
  // library the store never re-read, and would leave its object URLs to the
  // garbage collector that cannot reclaim them.
  GetSoundFiles: owned(SOUNDS_STORE, 'the sounds entity source'),
  PutSoundFile: owned(SOUNDS_STORE, 'addCustomSound()'),
  DeleteSoundFile: owned(SOUNDS_STORE, 'deleteCustomSound()'),
  SetAppearance: owned(APPEARANCE_STORE, 'setAppearance()'),
  SetWindowBackgroundColor: owned(APPEARANCE_STORE, 'syncWindowBackground()'),
  // The provider-account surface is one listing and one credential slot per
  // provider, and every mutation on it invalidates the others. A component
  // calling one of these directly would hold a listing the store never sees,
  // or start a sign-in the panel showing sign-ins knows nothing about.
  ListProviderAccounts: owned(PROVIDER_ACCOUNTS_STORE, 'runLoad()'),
  SwitchProviderAccount: owned(PROVIDER_ACCOUNTS_STORE, 'switchProviderAccount()'),
  RemoveProviderAccount: owned(PROVIDER_ACCOUNTS_STORE, 'removeProviderAccount()'),
  RefreshProviderAccountUsage: owned(
    PROVIDER_ACCOUNTS_STORE,
    'refreshProviderAccountUsage()',
  ),
  StartProviderLogin: owned(PROVIDER_ACCOUNTS_STORE, 'startProviderLogin()'),
  GetProviderLoginState: owned(PROVIDER_ACCOUNTS_STORE, 'hydrateProviderLogins()'),
  SubmitProviderLoginCode: owned(PROVIDER_ACCOUNTS_STORE, 'submitProviderLoginCode()'),
  CancelProviderLogin: owned(PROVIDER_ACCOUNTS_STORE, 'cancelProviderLogin()'),
  // The attached-machine list is one fact with a two-step pairing hanging
  // off it: a section calling AddBackend itself would show a verification
  // number the backend:attach frame has no way to retire.
  ListBackends: owned(SYSTEMS_STORE, 'loadSystems()'),
  AddBackend: owned(SYSTEMS_STORE, 'addSystem()'),
  RemoveBackend: owned(SYSTEMS_STORE, 'removeSystem()'),
  // Updating a supervised machine is one status box per backend fed by two
  // channels and one request. A card calling RequestServiceUpdate itself
  // would start a flow the box the card renders from never marked as its
  // own, and a second reader of GetServiceUpdateStatus would hold a status
  // the frames never converge.
  GetServiceUpdateStatus: owned(SERVICE_UPDATE_STORE, 'loadMachineUpdate()'),
  ListServiceReleases: owned(SERVICE_UPDATE_STORE, 'loadServiceReleases()'),
  RequestServiceUpdate: owned(SERVICE_UPDATE_STORE, 'requestServiceUpdate()'),
  // Provider CLI updates are one snapshot per computer fed by one channel.
  // A page calling RunProviderUpdate itself would start a run the Update
  // control never shows as busy, and a second reader of the snapshot would
  // hold a state the frames never converge.
  GetProviderUpdates: owned(PROVIDER_UPDATES_STORE, 'loadProviderUpdates()'),
  CheckProviderUpdates: owned(PROVIDER_UPDATES_STORE, 'checkProviderUpdates()'),
  GetProviderUpdateNotes: owned(PROVIDER_UPDATES_STORE, 'openProviderUpdateNotes()'),
  RunProviderUpdate: owned(PROVIDER_UPDATES_STORE, 'runProviderUpdate()'),
  // One machine's shareable ports is one list per backend fed by one
  // channel, and the two mutations reconcile against it. A markdown link
  // or a settings row calling AllowPreviewPort itself would share a port
  // the list the link renders from never learns about, and a second
  // reader of GetDevServers would answer from a snapshot the frames never
  // converge. MintPreviewURL spends a single-use ticket, so a caller
  // outside the store could mint one nothing opens.
  GetDevServers: owned(DEV_SERVERS_STORE, 'loadDevServers()'),
  AllowPreviewPort: owned(DEV_SERVERS_STORE, 'allowPreviewPort()'),
  DisallowPreviewPort: owned(DEV_SERVERS_STORE, 'disallowPreviewPort()'),
  MintPreviewURL: owned(DEV_SERVERS_STORE, 'openPreview()'),
  ListAvailableEditors: owned(EDITORS_STORE, 'startLoad()'),
  GetEditorSettings: owned(EDITORS_STORE, 'startLoad()'),
  SetEditorSettings: owned(EDITORS_STORE, 'setEditorPreference()'),
  // A computer's tracker connection list has one writer, its read, which
  // every action waits for; a caller of an action outside the store would
  // leave the list showing the state before it.
  TrackerConnections: owned(TRACKER_CONNECTIONS_STORE, 'HeldConnections.fetch()'),
  AddTrackerConnection: owned(TRACKER_CONNECTIONS_STORE, 'addTrackerConnection()'),
  RenameTrackerConnection: owned(TRACKER_CONNECTIONS_STORE, 'renameTrackerConnection()'),
  SetTrackerConnectionEnabled: owned(TRACKER_CONNECTIONS_STORE, 'setTrackerConnectionEnabled()'),
  SetTrackerConnectionTokenExpiry: owned(TRACKER_CONNECTIONS_STORE, 'setTrackerConnectionTokenExpiry()'),
  RemoveTrackerConnection: owned(TRACKER_CONNECTIONS_STORE, 'removeTrackerConnection()'),
  VerifyTrackerConnection: owned(TRACKER_CONNECTIONS_STORE, 'verifyTrackerConnection()'),
  EditTrackerKeyFile: owned(TRACKER_CONNECTIONS_STORE, 'editTrackerKeyFile()'),
  // A project's board list is written by its read and by its save's
  // answer, one save at a time.
  ProjectTrackerBoards: owned(TRACKER_PROJECT_BOARDS_STORE, 'HeldBoards.read()'),
  SetProjectTrackerBoards: owned(TRACKER_PROJECT_BOARDS_STORE, 'HeldBoards.save()'),
};

// Every home-routed browser binding the window can call, and how a call of
// it is checked against this page hosting the thread's browser
// (nativeBrowserRefusal) and the pane's attachment. A new one is added here
// with its check, and is routed through its owner above when it acts from a
// pane; the test below fails until it is.
const BROWSER_BINDING_CHECKS: Record<string, string> = {
  BrowserCompanionDo: 'sendFromPane / sendForThread: the host check and the pane attachment at the send',
  BrowserCompanionRecentAddresses: 'readRecentAddresses: the host check and the pane attachment at the send and at the answer',
  BrowserCompanionRevealPageFile: 'revealPageFile: the host check and the pane attachment at the send and at the answer',
  BrowserCompanionPaneAttach: 'the entity source refuses with nativeBrowserRefusal before it attaches',
  BrowserCompanionThreadState: 'a read, made only where canUseNativeBrowser, whose answer is admitted per observation',
  BrowserCompanionPaneDetach: 'the release of the mount its attach made: never refused, so no mount is left behind',
  BrowserCompanionPaneRect: 'sent for browserPaneMountId alone, which is null without a valid attachment',
  BrowserCompanionFind: 'a start names browserPaneMountId, null without a valid attachment; a stop is cleanup that must reach the backend',
  ClearBrowserSiteData: "a settings action on the home computer's browser, for no thread or pane: its scope is the check",
  VerifyBrowserUnlock: "the connected browser's own unlock, not the thread's browser",
};

// Every kind of action the window sends on the thread's browser, whether it
// takes a number in its pane mount's order, and why. A kind that reads or
// changes the tab strip's selection, order or membership is numbered, so no
// later one from the same pane overtakes it. A new kind is added here with
// its choice; the test below fails until the wire (NUMBERED_KINDS) and the
// backend's companionSpecs agree with it.
const NUMBERED_WIRE_KINDS: Record<string, readonly [numbered: boolean, why: string]> = {
  activate: [true, 'selects a tab'],
  step: [true, 'selects the tab beside the active one in strip order'],
  new: [true, 'adds a tab and selects it'],
  reopen: [true, 'adds the last closed tab at its place and selects it'],
  duplicate: [true, 'adds a tab beside the one it names and selects it'],
  'download-open': [true, 'adds a tab showing the file and selects it'],
  'show-browser': [true, 'opens a tab when the thread has none'],
  close: [true, 'takes a tab out and picks the next active one'],
  'close-others': [true, 'takes every other tab out'],
  'close-right': [true, 'takes out the tabs after the one it names, in strip order'],
  move: [true, 'reorders the strip'],
  back: [false, 'acts on the page it names'],
  forward: [false, 'acts on the page it names'],
  reload: [false, 'acts on the page it names'],
  stop: [false, 'acts on the page it names'],
  navigate: [false, 'acts on the page it names'],
  devtools: [false, 'acts on the page it names'],
  restore: [false, 'restores the tab it names only while it is still the active one, and selects nothing'],
  hide: [false, 'hides the pane and changes no tab'],
  jar: [false, "bound to its pane's mount; suspends and restores the tabs where they are"],
  'download-cancel': [false, 'acts on a download row'],
  'download-reveal': [false, 'acts on a download row'],
  'downloads-clear': [false, 'clears the download rows'],
  'focus-page': [false, 'a focus intent of the page it names'],
  'focus-page-withdraw': [false, 'withdraws a focus intent'],
};

// Kinds the backend accepts that the window never sends.
const BACKEND_ONLY_WIRE_KINDS = new Set(['show']);

// `stores/bindings.ts` is the typed wrapper the whole rule is phrased in terms
// of: it imports every generated RPC by definition, so it cannot be an
// offender without the rule contradicting itself.
const BINDINGS_WRAPPER = 'lib/stores/bindings.ts';
const OPEN_IN_EDITOR_OWNER = 'lib/stores/openInEditor.ts';

// Files that still reach an entity-owned RPC they do not own.
// Currently none — phases 1-3 moved every one of them behind a store, and the
// empty list is the claim that there are no exceptions left to grandfather.
const ENTITY_BINDING_ALLOWLIST: Record<string, string> = {};

// ---------------------------------------------------------------------------
// Rule 2 — wire subscriptions live in stores/.
//
// A `wailsEventOn` in a component is a second, unshared copy of whatever the
// event carries, with the component's lifetime instead of the entity's. Events
// are entity-keyed (see internal/transport/AGENTS.md), so the routing belongs
// once, in the store that owns that entity.
//
// `wailsEventOn` is the wrapper, not the door: `Events.On` from
// `@wailsio/runtime` (which the vite alias points at `lib/transport/runtime.ts`)
// is the same subscription one layer down, and a component reaching for it
// would pass a rule that only knew the wrapper's name.
//
// The exceptions below are all the same shape: the payload is consumed and
// dropped — a refetch trigger, a lifecycle signal, a byte stream written into a
// widget — so there is no entity state to share.
const WAILS_EVENT_ALLOWLIST: Record<string, string> = {
  'lib/components/composer/activityRailBackground.svelte.ts':
    'a transport gap on its thread is a rate-bounded refetch trigger for the tray; no payload state is kept (the tray\'s deltas arrive through stores/eventsBackgroundTray.ts)',
  'lib/components/composer/workspace/EnvPicker.svelte':
    'turn/background activity re-fetches the OPEN popover\'s worktree rows; the subscription lives and dies with the popover',
  'lib/components/takecontrol/TakeControlPane.svelte':
    'provider:session_died is this pane\'s own close signal — pane lifecycle, not shared state',
  'lib/components/takecontrol/TakeControlTerminal.svelte':
    'provider:terminal_output is a byte stream written straight into this pane\'s xterm; buffering it in a store would duplicate the terminal\'s scrollback',
};

// Authored promotion creates a second paint position outside the scrollTop
// chokepoint and can leave WebView2 presenting stale layer pixels. `will-change`
// is prohibited app-wide. ONE value is carved out below:
// `will-change: scroll-position` on the pane scroll surfaces (app.css
// `.pane-scroll-surface`) composits the scrollTop chokepoint's own mechanism —
// there is no second paint position, the scroll offset IS the chokepoint's
// value — and without it every offset change runs a full main-frame Layerize
// (measured 2026-08-25, user-approved same day). Transform state is
// additionally reviewed across the whole chat/virtual/scroll tree,
// not a list of today's plane files: adding a new row component must not
// create an enforcement blind spot.
const SCROLL_PRESENTATION_PREFIXES = [
  'lib/components/chat/',
  'lib/components/virtual/',
  'lib/utils/scroll/',
] as const;
const AUTHORIZED_SCROLL_PRESENTATION_STATE = [
  // Two ambient-indicator keyframes. Neither is scroll content: the owners are
  // a fixed-size SVG spinner glyph and the composer's one-frame sprite window,
  // both leaves outside the timeline planes. They animate transform precisely
  // so Blink runs them off the main thread — see the notes in app.css.
  'app.css|will-change declaration|will-change: scroll-position;',
  'app.css|transform declaration or keyframe|to { transform: rotate(360deg); }',
  'app.css|transform declaration or keyframe|to { transform: translateX(calc(-1 * var(--working-sprite-strip-w))); }',
  // The disclosure chevron of a <details> in review prose: a static quarter
  // turn on open, the same glyph CompactionDivider and
  // TranscriptDisclosureHeader rotate with `rotate-90`. No animation.
  'app.css|transform declaration or keyframe|.markdown-body.review-prose details[open] > summary::before {',
  'lib/components/chat/CompactionDivider.svelte|Tailwind transform utility|class:rotate-90={expanded}',
  // The pan and zoom of the two full-viewport viewers (utils/panZoom.svelte.ts):
  // one transform on the content host, outside any scroller.
  'lib/components/chat/DiagramModal.svelte|Svelte transform style directive|style:transform={view.transform}',
  'lib/components/chat/ExpandedImageDialog.svelte|Svelte transform style directive|style:transform={view.transform}',
  'lib/components/chat/ExpandedImageDialog.svelte|Tailwind transform utility|class="absolute left-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"',
  'lib/components/chat/ExpandedImageDialog.svelte|Tailwind transform utility|class="absolute right-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"',
  'lib/components/chat/MessageNavRail.svelte|Svelte transform style directive|style:transform={tickStyleTransform(i)}',
  "lib/components/chat/MessageNavRail.svelte|Svelte transform style directive|style:transform={previewCard?.translateY ?? 'none'}",
  'lib/components/chat/MessageNavRail.svelte|Tailwind transform utility|class="absolute h-[3px] w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-accent/75"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|Tailwind transform utility|class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 -translate-y-px border-x-[6px] border-t-[6px] border-x-transparent border-t-surface-2"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|Tailwind transform utility|class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 border-x-[7px] border-t-[7px] border-x-transparent border-t-border"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|transform declaration or keyframe|style={`top: ${pendingSelection.composerTop}px; left: ${pendingSelection.composerLeft}px; transform: translate(-50%, 0);`}',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|transform declaration or keyframe|style={`top: ${pendingSelection.triggerTop}px; left: ${pendingSelection.triggerLeft}px; transform: translate(-50%, -100%);`}',
  "lib/components/chat/TailClampedText.svelte|DOM transform property assignment|inner.style.transform = slideOffset > 0 ? `translateY(${slideOffset}px)` : '';",
  'lib/components/chat/TranscriptDisclosureHeader.svelte|Tailwind transform utility|class:rotate-90={expandable && expanded}',
  "lib/components/chat/messageNavRailSync.ts|DOM transform property assignment|strip.style.transform = clip > 0 ? `translateY(${-clip}px)` : '';",
] as const;
const PERFPROBE_SOURCES = import.meta.glob<string>('../../../scripts/perfprobe/**/*.mjs', { query: '?raw', import: 'default', eager: true });

// ---------------------------------------------------------------------------
// Rule 4 — the harness bridge is reachable only by dynamic import.
//
// `lib/harness/` is the agent test harness's in-page tooling: a document-wide
// MutationObserver, a rAF frame meter, a whitelisted read of the diagnostic
// globals. None of it may run in an ordinary boot, and the mechanism that
// guarantees that is bundling, not discipline — a STATIC import from anywhere
// in the app graph pulls the whole thing into the startup chunk, where it is
// fetched, parsed and evaluated on every launch by every user. The failure is
// silent: the modules do nothing until armed, so the only symptom is a bigger
// startup graph nobody looks at.
//
// The one door is the `import('../harness/bridge')` inside
// `stores/harnessBridge.ts`, which is why that file is the sole allowance and
// why the allowance is spelled as a DYNAMIC import: a static one from the same
// file would defeat the split just as thoroughly.
const HARNESS_DIR = join(SRC_ROOT, 'lib', 'harness');
const HARNESS_BRIDGE_OWNER = 'lib/stores/harnessBridge.ts';
// Files outside lib/harness/ that statically import from it. Empty, and it is
// the claim that the chunk boundary is real.
const HARNESS_STATIC_IMPORT_ALLOWLIST: Record<string, string> = {};

// ---------------------------------------------------------------------------
// Rule 4b — the Issues pane's stores load with the pane, not at startup.
//
// The board store, its model, the board choices, the pane's state, the
// ticket's store, model and writes, and the Time screen's weeks and
// writes hold visibility and connection listeners, a pump-vote queue and
// every rule of the board, the ticket and logged time, which a person who
// never opens Issues has no use for. The pane is the
// dynamic `import('../issues/IssuesPane.svelte')` in ToolPane.svelte, and
// the always-loaded stores reach those stores only through
// `stores/trackerBoardEvents.ts`, where they register when they load. A
// static import of one of them from anything the entry reaches folds it into
// the startup chunk. The rule walks the static, non-type imports from
// `main.ts`.
const APP_ENTRY = join(SRC_ROOT, 'main.ts');
const ISSUES_LAZY_MODULES = [
  'lib/stores/trackerBoard.svelte.ts',
  'lib/stores/trackerBoardModel.ts',
  'lib/stores/trackerBoardChoices.svelte.ts',
  'lib/stores/issuesPane.svelte.ts',
  'lib/stores/trackerTicket.svelte.ts',
  'lib/stores/trackerTicketModel.ts',
  'lib/stores/trackerTicketWrites.svelte.ts',
  'lib/stores/trackerTime.svelte.ts',
  'lib/stores/trackerTimeModel.ts',
  'lib/stores/trackerTimeWrites.svelte.ts',
  'lib/stores/trackerWriteOutcome.ts',
  'lib/utils/timeSpent.ts',
] as const;

// Rule 4c — the ticket's parts used by few load on first use: planning
// poker when its section opens, the link form and the Log time form when
// they open. The ticket is its own chunk (IssuesPane.svelte's dynamic
// import), and a static import from anything it reaches folds the part in.
const TICKET_SCREEN = join(SRC_ROOT, 'lib', 'components', 'issues', 'TicketScreen.svelte');
const TICKET_FIRST_USE_MODULES = [
  'lib/components/issues/TicketPoker.svelte',
  'lib/components/issues/TicketLinkForm.svelte',
  'lib/components/issues/WorklogForm.svelte',
] as const;

// ---------------------------------------------------------------------------
// Rule 5 — an event-driven refresh uses the scheduler, never a plain debounce.
//
// `utils/debounce` is a pure TRAILING debounce: every call clears the standing
// timer and arms a new one. Wrapped around a refresh driven by a provider event
// stream, that is a starvation bug and not a tuning choice — while events keep
// arriving closer together than the delay, the refresh is postponed forever and
// the surface shows whatever it last saw. Three modules had independently
// hand-rolled it (activity rail background tray, workspace-change lock, env
// picker worktree rows); in production on 2026-08-29 the Background pill read
// 10 against a truth of 3-4, and the other two GATE WORKSPACE MUTATION, where
// staleness is a safety defect.
//
// The rule is the conjunction, because neither half is wrong alone: a debounce
// with no event source is a quiet-edge persist (`paneLayoutPersistence`, which
// is meant to starve for the length of a drag), and an event subscription with
// no debounce has nothing to postpone.
const DEBOUNCE_MODULE = join(SRC_ROOT, 'lib', 'utils', 'debounce');
const REFRESH_SCHEDULER_MODULE = 'lib/utils/refreshScheduler.ts';
// The two ways a module takes a provider event stream. `wailsEventOn` is the
// wire subscription (rule 2 keeps it in stores/ or on its allowlist);
// `onItemUpsert` is the per-item fan-out, which is the higher-rate of the two
// and the one that starved the tray.
const EVENT_SUBSCRIPTION_IMPORTS = ['wailsEventOn', 'onItemUpsert'] as const;
// Empty, and that is the claim: every event-driven refresh in the tree is on
// the scheduler.
const DEBOUNCED_EVENT_REFRESH_ALLOWLIST: Record<string, string> = {};

// ---------------------------------------------------------------------------
// Rule 6 — random identifiers come from the one mint.
//
// `crypto.randomUUID` is a SECURE-CONTEXT API. A plain-HTTP LAN page is a
// shipped context for this app (docs/specs/remote-access.md §15 constraint 6:
// a phone reaching the desk over the LAN has no https and gets no secure
// context), and there the property is not merely restricted — it is ABSENT,
// so a call is a TypeError.
//
// That is a boot-time crash, not a degraded feature, which is why this one
// API gets a rule while its secure-context siblings do not. `wsClient`'s
// `generateId` mints the id of every RPC, so the throw landed on the first
// call of the boot fan-out and a freshly paired browser rendered a blank
// page — no error surface, because the code that would have drawn one had
// not mounted (found by the harness, 2026-08-31). `crypto.subtle` is absent
// on the same page and is fine: `transport/deviceKey.ts` feature-tests it
// and the device enrols with a bearer identifier instead. `navigator.clipboard`
// is absent too, and fails a CLICK rather than a launch.
//
// `lib/utils/randomId.ts` is the one place the fallback is written, and it
// falls back to `crypto.getRandomValues` — which is NOT secure-context gated,
// so the answer stays a CSPRNG rather than `Math.random` on the pages that
// need it most. The allowlist is empty and stays that way: a second call site
// is a second remembered answer, and the last four were four different ones.
const RANDOM_ID_OWNER = 'lib/utils/randomId.ts';
// A CALL, not the word: the comments at the converted call sites name the API
// they no longer reach for, and must not read as offenders.
const RANDOM_UUID_CALL = /\brandomUUID\s*\(/;
// The wire text of a dispatcher refusal from a release before the frame's
// "dispatch" field: a call it does not have, or one whose arguments did not
// fit. Only the legacy table in its owner matches it; everything else reads
// the transport's verdict, and the message is already the sentence.
const DISPATCH_REFUSAL_TEXT = /method not registered|params, got \d|too many parameters|bad parameter \d/;
const DISPATCH_REFUSAL_OWNER = 'lib/transport/dispatchRefusal.ts';
const RANDOM_ID_ALLOWLIST: Record<string, string> = {};

// 7. A send is built by the one builder that mints its idempotency id.
//
// A frame re-sent over a replacement socket is answered from the first
// arrival's record, and the id is the whole basis of that match. A send vector
// that assembles its own options object therefore looks like a NEW message on
// every retry — which is one turn started twice. `implementProposedPlan` was
// exactly that: an inline literal, no id, until this rule existed.
//
// The rule is on the IMPORT rather than the call, because the call's third
// argument is a variable at most sites and matching its provenance textually
// would be a guess. A module that reaches one of these RPCs either BUILDS its
// options (imports `buildSendOptions`) or was HANDED them already built (names
// `OutgoingSendOptions`, the only type that shape has) — a pass-through like
// `sendQueue.registerQueueItem` is the second. Inlining an object literal is
// neither, and that is the whole offence. The allowlist is empty.
const SEND_RPCS = ['SendMessageWithOptions', 'RegisterQueueItem'] as const;
const SEND_OPTIONS_BUILDER = 'buildSendOptions';
// A TEXT probe rather than an import name: the type arrives through
// `import type`, which `parseClause` deliberately drops as a non-runtime read.
const SEND_OPTIONS_TYPE = /\bOutgoingSendOptions\b/;
const SEND_OPTIONS_OWNER = 'lib/utils/sendOptions.ts';
const SEND_OPTIONS_ALLOWLIST: Record<string, string> = {};

// ---------------------------------------------------------------------------

interface ParsedImport {
  /** Module specifier as written. */
  readonly specifier: string;
  /** Named (non-type) bindings pulled in. */
  readonly names: readonly string[];
  /** `import * as x` or `import('…')` — grants the module's whole surface. */
  readonly wholeModule: boolean;
  /** `import('…')`, which puts the target in its own chunk rather than this one. */
  readonly dynamic: boolean;
  /** `import type` or `export type … from`: erased, so no edge of the module graph. */
  readonly typeOnly: boolean;
}

/**
 * The rules are about imports, so the walk is `.ts` / `.svelte` only.
 * `stores/` IS included; rule 1 scopes per owner and rule 2 filters it out.
 */
const SOURCE_EXTENSIONS = /\.(ts|svelte)$/;

/** Whether a specifier resolves to a module that re-exports generated RPCs. */
function isBindingsModule(fromFile: string, specifier: string): boolean {
  const local = resolveLocalModule(fromFile, specifier);
  if (local === null) return false;
  return local === BINDINGS_MODULE || local.startsWith(GENERATED_BINDINGS_DIR + sep);
}

// Anchored on the import/export keyword and stopped at the statement's `;`, so
// a non-greedy clause match can never run past the statement it started in.
const STATIC_IMPORT = /(?:^|[;{}\n])\s*(?:import|export)\s+([^;]*?)\s+from\s*(['"])([^'"]+)\2/g;
const SIDE_EFFECT_IMPORT = /(?:^|[;{}\n])\s*import\s*(['"])([^'"]+)\1/g;
const DYNAMIC_IMPORT = /\bimport\s*\(\s*(['"])([^'"]+)\1\s*\)/g;

function parseImports(source: string): ParsedImport[] {
  const imports: ParsedImport[] = [];
  for (const match of source.matchAll(STATIC_IMPORT)) {
    imports.push({ specifier: match[3]!, ...parseClause(match[1]!), dynamic: false, typeOnly: isTypeOnlyClause(match[1]!) });
  }
  for (const match of source.matchAll(SIDE_EFFECT_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: false, dynamic: false, typeOnly: false });
  }
  for (const match of source.matchAll(DYNAMIC_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: true, dynamic: true, typeOnly: false });
  }
  return imports;
}

interface ParsedExports {
  /** Exported name → the specifier and name it is re-exported from ('*' for a namespace). */
  readonly reexports: ReadonlyMap<string, { readonly specifier: string; readonly name: string }>;
  /** `export * from` specifiers. */
  readonly stars: readonly string[];
  /** Names this module declares and exports itself. */
  readonly declares: ReadonlySet<string>;
}

const REEXPORT_NAMED = /(?:^|[;{}\n])\s*export\s*(type\s+)?\{([^}]*)\}\s*from\s*(['"])([^'"]+)\3/g;
const REEXPORT_STAR = /(?:^|[;{}\n])\s*export\s*\*\s*(?:as\s+([A-Za-z_$][\w$]*)\s*)?from\s*(['"])([^'"]+)\2/g;
const EXPORT_LOCAL = /(?:^|[;{}\n])\s*export\s*(type\s+)?\{([^}]*)\}(?!\s*from)/g;
const EXPORT_DECLARED = /(?:^|[;{}\n])\s*export\s+(?:declare\s+)?(?:async\s+)?(?:function\*?|const|let|var|class|enum)\s+([A-Za-z_$][\w$]*)/g;
const IMPORT_BINDINGS = /(?:^|[;{}\n])\s*import\s+(?!type\s)([^;]*?)\s+from\s*(['"])([^'"]+)\2/g;

/** `a as b` pieces of a brace list, type-only pieces dropped: [local, exported]. */
function bracePieces(list: string): [string, string][] {
  const out: [string, string][] = [];
  for (const raw of list.split(',')) {
    const piece = raw.trim();
    if (!piece || /^type\s/.test(piece)) continue;
    const [local, exported] = piece.split(/\s+as\s+/).map((part) => part.trim());
    if (local) out.push([local, exported ?? local]);
  }
  return out;
}

function parseExports(source: string): ParsedExports {
  const reexports = new Map<string, { specifier: string; name: string }>();
  const stars: string[] = [];
  const declares = new Set<string>();
  for (const match of source.matchAll(REEXPORT_NAMED)) {
    if (match[1]) continue;
    for (const [local, exported] of bracePieces(match[2]!)) reexports.set(exported, { specifier: match[4]!, name: local });
  }
  for (const match of source.matchAll(REEXPORT_STAR)) {
    if (match[1]) reexports.set(match[1], { specifier: match[3]!, name: '*' });
    else stars.push(match[3]!);
  }
  // A binding this module imported and exports again is a re-export too.
  const imported = new Map<string, { specifier: string; name: string }>();
  for (const match of source.matchAll(IMPORT_BINDINGS)) {
    const clause = match[1]!.trim();
    const specifier = match[3]!;
    const namespace = clause.match(/\*\s+as\s+([A-Za-z_$][\w$]*)/);
    if (namespace) imported.set(namespace[1]!, { specifier, name: '*' });
    const braces = clause.match(/\{([\s\S]*)\}/);
    if (braces) for (const [name, local] of bracePieces(braces[1]!)) imported.set(local, { specifier, name });
    const fallback = clause.match(/^([A-Za-z_$][\w$]*)\s*(?:,|$)/);
    if (fallback) imported.set(fallback[1]!, { specifier, name: 'default' });
  }
  for (const match of source.matchAll(EXPORT_LOCAL)) {
    if (match[1]) continue;
    for (const [local, exported] of bracePieces(match[2]!)) {
      const from = imported.get(local);
      if (from) reexports.set(exported, from);
      else declares.add(exported);
    }
  }
  for (const match of source.matchAll(EXPORT_DECLARED)) declares.add(match[1]!);
  return { reexports, stars, declares };
}

function parseClause(clause: string): { names: string[]; wholeModule: boolean } {
  const trimmed = clause.trim();
  if (/(^|,)\s*\*\s+as\s+/.test(trimmed)) return { names: [], wholeModule: true };
  // `import type { … }` brings in no runtime value, so it cannot be a state read.
  const typeOnlyStatement = /^type\b/.test(trimmed);
  const braces = trimmed.match(/\{([\s\S]*)\}/);
  if (!braces) return { names: [], wholeModule: false };
  const names: string[] = [];
  for (const raw of braces[1]!.split(',')) {
    const piece = raw.trim();
    if (!piece || typeOnlyStatement || /^type\s/.test(piece)) continue;
    const name = piece.split(/\s+as\s+/)[0]!.trim();
    if (name) names.push(name);
  }
  return { names, wholeModule: false };
}

/** Whether an import or re-export clause brings in no runtime value: `type
 * { … }`, or braces whose every piece is `type X`. A default or namespace
 * binding beside them is a value. */
function isTypeOnlyClause(clause: string): boolean {
  const trimmed = clause.trim();
  if (/^type\b/.test(trimmed)) return true;
  const braces = trimmed.match(/^\{([\s\S]*)\}$/);
  if (!braces) return false;
  const pieces = braces[1]!.split(',').map((piece) => piece.trim()).filter(Boolean);
  return pieces.length > 0 && pieces.every((piece) => /^type\s/.test(piece));
}

/** Absolute, extensionless path a specifier points at, or null when external. */
function resolveLocalModule(fromFile: string, specifier: string): string | null {
  let path: string;
  if (specifier.startsWith('.')) {
    path = resolve(dirname(fromFile), specifier);
  } else if (specifier.startsWith('~/')) {
    // tsconfig maps `~/*` to `src/*`.
    path = join(SRC_ROOT, specifier.slice(2));
  } else {
    return null;
  }
  return path.replace(/\.(ts|js|svelte)$/, '');
}

// Rule: under components/sidebar/, only the entry projection reads the
// projects store and the sidebar's entry continuity.
//
// A repository checked out on several computers is one sidebar row, so what
// that row shows is a fact about the entry and not about any one checkout.
// `sidebarEntries.ts` builds the entry value from those two stores, and a
// new fact about a row is added to that value instead of derived beside it.
// The rule is by module and default-deny: from either store a file here may
// import only the names listed below for it, and everything else, a name
// added later included, fails. Each import is resolved through re-exports
// (`export { x } from`, `export *`, and an imported binding exported again)
// to the module that declares it, so another module re-exporting a read
// does not open a way around. What the rule does not see is a function in
// another module that calls a read and returns its answer.
const SIDEBAR_DIR = join(SRC_ROOT, 'lib', 'components', 'sidebar');
const PROJECTS_STORE = join(STORES_DIR, 'projects');
const ENTRY_CONTINUITY_STORE = join(STORES_DIR, 'sidebarEntryContinuity');
const SIDEBAR_ENTRY_OWNER = 'lib/components/sidebar/sidebarEntries.ts';
const ANY_SIDEBAR_FILE = '*';
// Fenced module → name → the files under components/sidebar that may import
// it, or ANY_SIDEBAR_FILE. Writers only: none of them answers a question
// about a row.
const SIDEBAR_FENCED_IMPORTS: ReadonlyMap<string, Readonly<Record<string, string>>> = new Map<string, Readonly<Record<string, string>>>([
  [PROJECTS_STORE, {
    updateProjectLocal: ANY_SIDEBAR_FILE,
    removeProjectLocal: ANY_SIDEBAR_FILE,
    // The delete action confirms a deletion, which forgets the entry's disclosure.
    deleteProjectLocal: 'lib/components/sidebar/ProjectContextMenu.svelte',
    reorderProjects: ANY_SIDEBAR_FILE,
  }],
  [ENTRY_CONTINUITY_STORE, {
    revealProject: ANY_SIDEBAR_FILE,
    // A row reports a toggle to the list that has the entry rather than
    // writing its disclosure itself.
    setEntryExpanded: 'lib/components/sidebar/ProjectsSection.svelte',
    setEntryListLimit: 'lib/components/sidebar/SidebarTreeList.svelte',
    resetEntryListLimit: 'lib/components/sidebar/SidebarTreeList.svelte',
  }],
]);
// Shrink-only. Project-level surfaces that happen to live in this directory,
// not readers of the sidebar tree.
const SIDEBAR_PROJECT_READ_ALLOWLIST: Record<string, string> = {
  'lib/components/sidebar/AssistantProjectsPicker.svelte':
    'ticks the projects one assistant serves, which is a list of projects and not the sidebar tree',
  'lib/components/sidebar/AssistantsSection.svelte':
    "labels the projects an assistant serves on its card, which is not a project row's label",
};

// Rule: under components/sidebar/, a group's and the Assistants section's list
// sizes are read and written by the list that renders them.
//
// A row reports a "Show more" or "Show less" to the list, which owns the
// limit it renders by. Resolved through re-exports like the rule above. A
// project's own list sizes and folds are the next rule's.
const THREAD_LIST_SIZES: Record<string, string> = {
  getThreadListVisibleLimit: 'lib/components/sidebar/SidebarTreeList.svelte',
  setThreadListVisibleLimit: 'lib/components/sidebar/SidebarTreeList.svelte',
  collapseThreadList: 'lib/components/sidebar/SidebarTreeList.svelte',
};
const SIDEBAR_STORE = join(STORES_DIR, 'sidebar');
// Shrink-only, and empty.
const SIDEBAR_LIST_SIZE_ALLOWLIST: Record<string, string> = {};

// Rule: a disclosure record is made only by the store that keeps it.
//
// A record constructor takes any storage key, so a module that made one
// under a key another store keeps would write over that store's record. The
// sidebar store makes the groups', the sections' and the Assistants' records;
// the entry continuity store makes a project entry's. Default-deny over every
// source outside tests: only the continuity store may import a constructor,
// resolved through re-exports like the rules above, and a whole-module import
// that reaches the sidebar store is refused anywhere else. What the rule does
// not see is a write through appStorage under the same key.
const RECORD_CONSTRUCTORS = new Set(['keySetRecord', 'listLimitRecord']);
const RECORD_CONSTRUCTOR_IMPORTER = 'lib/stores/sidebarEntryContinuity.svelte.ts';
// Shrink-only, and empty.
const RECORD_CONSTRUCTOR_ALLOWLIST: Record<string, string> = {};

// Rule: a computer's kept catalog records are written only by its write queue.
//
// Every write of the three records is the computer's whole tree, made by the
// one queue per computer (stores/computerCatalogWriter.ts), which writes only
// while the computer's three catalogs are one tree and only under the stamps
// this window holds. A second caller could write catalogs that were never
// held together, or a request the queue is still holding. Default-deny over
// every source outside tests: only the queue may import the write, resolved
// through re-exports like the rules above, and a whole-module import that
// reaches the replica session is refused anywhere else.
const REPLICA_CATALOG_WRITES = new Set(['putReplicaTree']);
const REPLICA_SESSION = join(SRC_ROOT, 'lib', 'replica', 'session');
const REPLICA_CATALOG_WRITER = 'lib/stores/computerCatalogWriter.ts';
// Shrink-only, and empty.
const REPLICA_CATALOG_WRITE_ALLOWLIST: Record<string, string> = {};

// A call, not the words: the comments in lib/transport/ discuss `Events.On`
// by name and must not read as subscriptions.
const EVENTS_ON_CALL = /\bEvents\s*\.\s*On\s*\(/;

describe('architecture', () => {
  // Each file's text is read, reduced to the two facts the rules need, and
  // dropped — holding ~800 whole sources for the lifetime of the suite bought
  // nothing but resident bytes.
  const sources = SCANNED.files(SOURCE_EXTENSIONS).map((file) => {
    const text = SCANNED.text(file);
    return {
      file,
      path: repoPath(file),
      imports: parseImports(text),
      exports: parseExports(text),
      callsEventsOn: EVENTS_ON_CALL.test(text),
      callsRandomUUID: RANDOM_UUID_CALL.test(text),
      namesSendOptionsType: SEND_OPTIONS_TYPE.test(text),
      namesDispatchRefusalText: DISPATCH_REFUSAL_TEXT.test(text),
      inStores: file.startsWith(STORES_DIR + sep),
    };
  });

  it('keeps every entity-owned RPC with the store that owns it', () => {
    const offenders = new Map<string, string[]>();
    for (const source of sources) {
      if (source.path === BINDINGS_WRAPPER) continue;
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const fromBindings = isBindingsModule(source.file, parsed.specifier);
        if (fromBindings && parsed.wholeModule) {
          reasons.push(
            `imports the whole ${parsed.specifier} surface, which includes every entity-owned RPC`,
          );
          continue;
        }
        // Names are matched on ANY specifier, not just stores/bindings: the
        // generated `bindings/agent-overflow/app` module exports the same
        // functions, and a rule that only knew the wrapper would wave through
        // the one import path that skips it.
        for (const name of parsed.names) {
          const owner = ENTITY_OWNED_BINDINGS[name];
          if (!owner || owner.owner === source.path) continue;
          reasons.push(`${name} is owned by ${owner.owner} — ${owner.where}`);
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    expectAllowlistExact(
      offenders,
      ENTITY_BINDING_ALLOWLIST,
      'New violations.',
      'Read the entity through its store instead of re-fetching it; see frontend/CLAUDE.md → State Boundaries.',
    );
  });

  // Every module's exports, by its extensionless path, for resolving an
  // import to the module that declares it.
  const exportsByModule = new Map(sources.map((source) => [
    source.file.replace(/(\.svelte)?\.(ts|js|svelte)$/, ''),
    { file: source.file, ...source.exports },
  ]));

  /** The module that declares `name` as `module` exports it, through re-exports. */
  function originOf(module: string, name: string, seen = new Set<string>()): { module: string; name: string } {
    const key = `${module}#${name}`;
    if (seen.has(key)) return { module, name };
    seen.add(key);
    const info = exportsByModule.get(module);
    if (info === undefined) return { module, name };
    const named = info.reexports.get(name);
    if (named !== undefined) {
      const target = resolveLocalModule(info.file, named.specifier);
      if (target === null) return { module, name };
      return named.name === '*' ? { module: target, name: '*' } : originOf(target, named.name, seen);
    }
    if (info.declares.has(name)) return { module, name };
    for (const specifier of info.stars) {
      const target = resolveLocalModule(info.file, specifier);
      if (target === null) continue;
      const found = originOf(target, name, seen);
      if (found.module !== target || exportsByModule.get(target)?.declares.has(name)) return found;
    }
    return { module, name };
  }

  /** Which of `fenced` a whole-module import of `module` reaches. */
  function reachedBehind(module: string, fenced: ReadonlySet<string>, seen = new Set<string>()): string[] {
    if (fenced.has(module)) return [module];
    if (seen.has(module)) return [];
    seen.add(module);
    const info = exportsByModule.get(module);
    if (info === undefined) return [];
    const reached = new Set<string>();
    for (const name of info.reexports.keys()) {
      const origin = originOf(module, name);
      if (fenced.has(origin.module)) reached.add(origin.module);
    }
    for (const specifier of info.stars) {
      const target = resolveLocalModule(info.file, specifier);
      if (target !== null) for (const found of reachedBehind(target, fenced, seen)) reached.add(found);
    }
    return [...reached];
  }
  const FENCED_STORES = new Set(SIDEBAR_FENCED_IMPORTS.keys());

  it('keeps every read of the projects and their entries in the sidebar entry projection', () => {
    const offenders = new Map<string, string[]>();
    const ownerReads = new Set<string>();
    for (const source of sources) {
      if (!source.file.startsWith(SIDEBAR_DIR + sep)) continue;
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const module = resolveLocalModule(source.file, parsed.specifier);
        if (module === null) continue;
        if (parsed.wholeModule) {
          for (const fenced of reachedBehind(module, FENCED_STORES)) {
            if (source.path === SIDEBAR_ENTRY_OWNER) ownerReads.add(fenced);
            else reasons.push(`imports the whole of ${parsed.specifier}, which reaches ${repoPath(fenced)}`);
          }
          continue;
        }
        for (const name of parsed.names) {
          const origin = originOf(module, name);
          const allowed = SIDEBAR_FENCED_IMPORTS.get(origin.module);
          if (allowed === undefined) continue;
          if (source.path === SIDEBAR_ENTRY_OWNER) {
            ownerReads.add(origin.module);
            continue;
          }
          const who = allowed[origin.name];
          if (who === ANY_SIDEBAR_FILE || who === source.path) continue;
          reasons.push(who === undefined
            ? `${name} (${repoPath(origin.module)}#${origin.name}) is not one of the names this directory may import from there`
            : `${name} (${repoPath(origin.module)}#${origin.name}) is ${who}'s to import`);
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    // The projection must still be what reads both stores: with its reads
    // gone the rule would hold over a directory that had found another way.
    expect([...ownerReads].map(repoPath).sort(), `${SIDEBAR_ENTRY_OWNER} reads the stores it projects`)
      .toEqual([...SIDEBAR_FENCED_IMPORTS.keys()].map(repoPath).sort());
    expectAllowlistExact(
      offenders,
      SIDEBAR_PROJECT_READ_ALLOWLIST,
      'New violations.',
      'Take the fact off the SidebarEntry the component is given (components/sidebar/sidebarEntries.ts); add it there if it is missing.',
    );
  });

  it("keeps a group's and the Assistants section's list sizes with the list that renders them", () => {
    const sidebarStore = new Set([SIDEBAR_STORE]);
    const offenders = new Map<string, string[]>();
    const owners = new Set<string>();
    for (const source of sources) {
      if (!source.file.startsWith(SIDEBAR_DIR + sep)) continue;
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const module = resolveLocalModule(source.file, parsed.specifier);
        if (module === null) continue;
        if (parsed.wholeModule) {
          if (reachedBehind(module, sidebarStore).length > 0) {
            reasons.push(`imports the whole of ${parsed.specifier}, which reaches the list sizes`);
          }
          continue;
        }
        for (const name of parsed.names) {
          const origin = originOf(module, name);
          if (origin.module !== SIDEBAR_STORE) continue;
          const owner = THREAD_LIST_SIZES[origin.name];
          if (owner === undefined) continue;
          if (owner === source.path) {
            owners.add(origin.name);
            continue;
          }
          reasons.push(`${name} (${repoPath(origin.module)}#${origin.name}) is ${owner}'s to read and write`);
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    // The list still holds its names: with nothing importing them, this rule
    // would hold over a directory that had moved the sizes somewhere else.
    expect([...owners].sort()).toEqual(Object.keys(THREAD_LIST_SIZES).sort());
    expectAllowlistExact(
      offenders,
      SIDEBAR_LIST_SIZE_ALLOWLIST,
      'New violations.',
      'Report the change to the list that renders the rows (SidebarTreeList.svelte).',
    );
  });

  it('makes each disclosure record only in the store that keeps it', () => {
    const sidebarStore = new Set([SIDEBAR_STORE]);
    const offenders = new Map<string, string[]>();
    const imported = new Set<string>();
    for (const source of sources) {
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const module = resolveLocalModule(source.file, parsed.specifier);
        if (module === null) continue;
        if (parsed.wholeModule) {
          if (source.path !== RECORD_CONSTRUCTOR_IMPORTER && reachedBehind(module, sidebarStore).length > 0) {
            reasons.push(`imports the whole of ${parsed.specifier}, which reaches the record constructors`);
          }
          continue;
        }
        for (const name of parsed.names) {
          const origin = originOf(module, name);
          if (origin.module !== SIDEBAR_STORE || !RECORD_CONSTRUCTORS.has(origin.name)) continue;
          if (source.path === RECORD_CONSTRUCTOR_IMPORTER) {
            imported.add(origin.name);
            continue;
          }
          reasons.push(`${name} (${repoPath(origin.module)}#${origin.name}) makes a record only its keeper may make`);
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    // The continuity store still makes its records with them: with its
    // imports gone the rule would hold over records made some other way.
    expect([...imported].sort()).toEqual([...RECORD_CONSTRUCTORS].sort());
    expectAllowlistExact(
      offenders,
      RECORD_CONSTRUCTOR_ALLOWLIST,
      'New violations.',
      'Read or write the disclosure through the store that keeps the record (sidebar.svelte.ts or sidebarEntryContinuity.svelte.ts).',
    );
  });

  it("writes a computer's kept catalogs only through its write queue", () => {
    const session = new Set([REPLICA_SESSION]);
    const offenders = new Map<string, string[]>();
    const imported = new Set<string>();
    for (const source of sources) {
      if (source.path === repoPath(`${REPLICA_SESSION}.ts`)) continue;
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const module = resolveLocalModule(source.file, parsed.specifier);
        if (module === null) continue;
        if (parsed.wholeModule) {
          if (source.path !== REPLICA_CATALOG_WRITER && reachedBehind(module, session).length > 0) {
            reasons.push(`imports the whole of ${parsed.specifier}, which reaches the catalog writes`);
          }
          continue;
        }
        for (const name of parsed.names) {
          const origin = originOf(module, name);
          if (origin.module !== REPLICA_SESSION || !REPLICA_CATALOG_WRITES.has(origin.name)) continue;
          if (source.path === REPLICA_CATALOG_WRITER) {
            imported.add(origin.name);
            continue;
          }
          reasons.push(`${name} (${repoPath(origin.module)}#${origin.name}) writes a kept catalog outside its computer's queue`);
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    // The queue still writes through them: with its imports gone the rule
    // would hold over records written some other way.
    expect([...imported].sort()).toEqual([...REPLICA_CATALOG_WRITES].sort());
    expectAllowlistExact(
      offenders,
      REPLICA_CATALOG_WRITE_ALLOWLIST,
      'New violations.',
      "Change a catalog through its store's writer, or ask for a tree with persistCatalogTree (computerCatalogWriter.ts).",
    );
  });

  it('names the check of every browser binding the window can call', () => {
    const routes = SCANNED.text(join(SRC_ROOT, 'lib', 'transport', 'methodRoutes.ts'));
    const methods = new Set([...routes.matchAll(/\/\/ (\w+)$/gm)].map((match) => match[1]));
    const wrapper = SCANNED.text(join(SRC_ROOT, BINDINGS_WRAPPER));
    const exported = [...wrapper.matchAll(/^\s+(\w*Browser\w*),?$/gm)].map((match) => match[1]);
    const browserBindings = exported.filter((name) => methods.has(name)).sort();
    expect(browserBindings).toEqual(Object.keys(BROWSER_BINDING_CHECKS).sort());
    // The pane's calls on the thread's browser go through the one send path.
    for (const name of ['BrowserCompanionDo', 'BrowserCompanionRecentAddresses', 'BrowserCompanionRevealPageFile']) {
      expect(ENTITY_OWNED_BINDINGS[name]?.owner, name).toBe('lib/stores/browserActionWire.ts');
    }
  });

  it('numbers every wire kind that reads or changes the tab strip, on both sides', () => {
    const decided = Object.fromEntries(Object.entries(NUMBERED_WIRE_KINDS).map(([kind, [numbered]]) => [kind, numbered]));
    expect({ ...NUMBERED_KINDS }).toEqual(decided);
    const specs = browserCompanionActions;
    const backend = new Map(
      [...specs.matchAll(/^\t+"([a-z-]+)":\s+\{kind: kind\w+[^}]*\}/gm)].map((match) => [
        match[1],
        /place: place(Numbered|IfFromPane)\b/.test(match[0]),
      ]),
    );
    expect(backend.size).toBeGreaterThan(20);
    for (const kind of BACKEND_ONLY_WIRE_KINDS) backend.delete(kind);
    expect(Object.fromEntries(backend)).toEqual(decided);
  });

  it('keeps OpenInEditor behind its view-only gate', () => {
    const offenders = new Map<string, string[]>();
    for (const source of sources) {
      if (source.path === BINDINGS_WRAPPER || source.path === OPEN_IN_EDITOR_OWNER) continue;
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        if (!isBindingsModule(source.file, parsed.specifier)) continue;
        if (parsed.wholeModule || parsed.names.includes('OpenInEditor')) {
          reasons.push(
            `imports OpenInEditor from '${parsed.specifier}' instead of the view-only-gated owner`,
          );
        }
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      `Call openInEditor() from ${OPEN_IN_EDITOR_OWNER} so view-only sessions cannot reach the host-scoped RPC.`,
    );
  });

  it('keeps wire subscriptions inside stores/', () => {
    const offenders = new Map<string, string[]>();
    for (const source of sources) {
      if (source.inStores) continue;
      const reasons = source.imports
        .filter((parsed) => parsed.names.includes('wailsEventOn'))
        .map((parsed) => `subscribes to backend events via wailsEventOn (from '${parsed.specifier}')`);
      for (const parsed of source.imports) {
        if (parsed.specifier !== '@wailsio/runtime') continue;
        reasons.push('imports @wailsio/runtime directly, which carries the raw Events surface');
      }
      if (source.callsEventsOn) {
        reasons.push('calls Events.On, the raw subscription behind wailsEventOn');
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    expectAllowlistExact(
      offenders,
      WAILS_EVENT_ALLOWLIST,
      'New violations.',
      'Route the event through the store that owns the entity it describes; see frontend/CLAUDE.md → State Boundaries.',
    );
  });

  it('keeps lib/harness/ behind its one dynamic import', () => {
    const offenders = new Map<string, string[]>();
    let door = 0;
    for (const source of sources) {
      const inHarness = source.file.startsWith(HARNESS_DIR + sep);
      const reasons: string[] = [];
      for (const parsed of source.imports) {
        const local = resolveLocalModule(source.file, parsed.specifier);
        if (local === null || !local.startsWith(HARNESS_DIR + sep)) continue;
        if (parsed.dynamic) {
          if (source.path === HARNESS_BRIDGE_OWNER) door += 1;
          continue;
        }
        if (inHarness) continue;
        reasons.push(
          `statically imports '${parsed.specifier}', which pulls the harness chunk into the startup graph`,
        );
      }
      if (reasons.length > 0) offenders.set(source.path, reasons);
    }
    // Without this the rule would keep passing after the door itself was
    // deleted or renamed, and a passing rule over a tree it no longer
    // describes is worse than no rule.
    expect(door, `${HARNESS_BRIDGE_OWNER} must reach the bridge by import()`).toBeGreaterThan(0);
    expectAllowlistExact(
      offenders,
      HARNESS_STATIC_IMPORT_ALLOWLIST,
      'New violations.',
      `Reach the bridge through the dynamic import in ${HARNESS_BRIDGE_OWNER}; see frontend/CLAUDE.md → Layout, src/lib/harness/.`,
    );
  });

  /** The modules `entryFile`'s static, non-type imports reach, each with
   * the chain that reached it first. */
  function staticGraph(entryFile: string): { reached: (path: string) => boolean; chain: (path: string) => string } {
    // An import names a module without its extension, and `x.svelte.ts` as
    // `x.svelte`: every name a file can be imported by leads to it.
    const byModule = new Map<string, (typeof sources)[number][]>();
    for (const source of sources) {
      const names = [source.file.replace(/\.(ts|svelte)$/, '')];
      if (source.file.endsWith('.svelte.ts')) names.push(source.file.slice(0, -'.svelte.ts'.length));
      for (const name of names) byModule.set(name, [...(byModule.get(name) ?? []), source]);
    }
    const entry = sources.find((source) => source.file === entryFile);
    expect(entry, `${entryFile} is where the walk starts`).toBeDefined();
    // Each module reached, with the module that reached it first.
    const reachedFrom = new Map<string, string>([[entry!.path, '']]);
    const queue = [entry!];
    while (queue.length > 0) {
      const source = queue.shift()!;
      for (const parsed of source.imports) {
        if (parsed.dynamic || parsed.typeOnly) continue;
        const local = resolveLocalModule(source.file, parsed.specifier);
        if (local === null) continue;
        for (const target of byModule.get(local) ?? []) {
          if (reachedFrom.has(target.path)) continue;
          reachedFrom.set(target.path, source.path);
          queue.push(target);
        }
      }
    }
    return {
      reached: (path) => reachedFrom.has(path),
      chain: (path) => {
        const out = [path];
        for (let at = reachedFrom.get(path); at; at = reachedFrom.get(at)) out.push(at);
        return out.reverse().join(' → ');
      },
    };
  }

  it('keeps the Issues stores out of the startup graph', () => {
    const graph = staticGraph(APP_ENTRY);
    // Without this the rule would pass over a walk that reached nothing.
    expect(graph.reached('lib/stores/trackerBoardEvents.ts'), 'the walk reaches the registration module').toBe(true);
    const offenders = ISSUES_LAZY_MODULES.filter(graph.reached).map(graph.chain);
    expect(
      offenders,
      'An Issues store is in the startup graph. Reach it through stores/trackerBoardEvents.ts or the pane\'s dynamic import; see frontend/CLAUDE.md → Performance-sensitive rules.',
    ).toEqual([]);
  });

  it('keeps planning poker, the link form and the Log time form out of the ticket\'s chunk', () => {
    const graph = staticGraph(TICKET_SCREEN);
    expect(graph.reached('lib/components/issues/TicketLinks.svelte'), 'the walk reaches the linked tickets').toBe(true);
    const offenders = TICKET_FIRST_USE_MODULES.filter(graph.reached).map(graph.chain);
    expect(
      offenders,
      'A ticket part that loads on first use is in the ticket\'s chunk. Load it with a dynamic import at its section or form; see frontend/CLAUDE.md → Performance-sensitive rules.',
    ).toEqual([]);
  });

  it('keeps the week cache out of what the log form imports', () => {
    // A ticket's Log time loads the log form; the weeks the Time screen
    // holds, and their listeners, load with the Time screen alone.
    const graph = staticGraph(join(SRC_ROOT, 'lib', 'components', 'issues', 'WorklogForm.svelte'));
    expect(graph.reached('lib/stores/trackerTimeWrites.svelte.ts'), 'the walk reaches the worklog writes').toBe(true);
    const weeks = 'lib/stores/trackerTime.svelte.ts';
    expect(
      graph.reached(weeks) ? [graph.chain(weeks)] : [],
      'The log form imports the week cache; the Time host owns the weeks and their invalidation.',
    ).toEqual([]);
  });

  it('keeps event-driven refresh on the rate-bounded scheduler', () => {
    const offenders = new Map<string, string[]>();
    let debounceImporters = 0;
    for (const source of sources) {
      const debounces = source.imports.some(
        (parsed) =>
          resolveLocalModule(source.file, parsed.specifier) === DEBOUNCE_MODULE
          && (parsed.wholeModule || parsed.names.includes('debounce')),
      );
      if (!debounces) continue;
      debounceImporters += 1;
      const subscriptions: string[] = [];
      for (const parsed of source.imports) {
        for (const name of EVENT_SUBSCRIPTION_IMPORTS) {
          if (parsed.names.includes(name)) subscriptions.push(`${name} (from '${parsed.specifier}')`);
        }
      }
      if (source.callsEventsOn) subscriptions.push('Events.On');
      if (subscriptions.length === 0) continue;
      offenders.set(source.path, [
        `debounces while subscribing to ${subscriptions.join(', ')} — a trailing`
        + ' debounce restarts its timer on every event, so a stream that never'
        + ' pauses postpones the refresh forever',
      ]);
    }
    // Without this the rule would keep passing after utils/debounce moved or
    // was renamed — a rule scanning for a module nobody imports finds nothing
    // and says so cheerfully.
    expect(
      debounceImporters,
      `no module imports ${repoPath(DEBOUNCE_MODULE)}; this rule is scanning for nothing`,
    ).toBeGreaterThan(0);
    expectAllowlistExact(
      offenders,
      DEBOUNCED_EVENT_REFRESH_ALLOWLIST,
      'New violations.',
      `Drive the refresh with ${REFRESH_SCHEDULER_MODULE}, whose absolute deadline fires`
      + ' under a stream that never goes quiet. Plain debounce stays correct only for a'
      + ' quiet-edge persist that nothing reads mid-burst.',
    );
  });

  it('keeps random identifiers on the secure-context-safe mint', () => {
    const offenders = new Map<string, string[]>();
    for (const source of sources) {
      if (source.path === RANDOM_ID_OWNER) continue;
      if (!source.callsRandomUUID) continue;
      offenders.set(source.path, [
        'calls crypto.randomUUID(), which is absent on a plain-HTTP LAN page and throws there',
      ]);
    }
    expectAllowlistExact(
      offenders,
      RANDOM_ID_ALLOWLIST,
      'New violations.',
      `Call randomId() from ${RANDOM_ID_OWNER}, which falls back to crypto.getRandomValues`
      + ' on the pages where crypto.randomUUID does not exist.',
    );
  });

  it('words a refusal by the computer\'s dispatcher only through the transport', () => {
    const offenders = new Map<string, string[]>();
    for (const source of sources) {
      if (!source.namesDispatchRefusalText || source.path === DISPATCH_REFUSAL_OWNER) continue;
      offenders.set(source.path, ['matches or shows the wire text of a dispatcher refusal']);
    }
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Branch on the code with isMethodUnavailableError; the error\'s message is already the'
      + ' sentence for the refusal (transport/dispatchRefusal.ts).',
    );
    expect(sources.some((source) => source.path === DISPATCH_REFUSAL_OWNER && source.namesDispatchRefusalText), `${DISPATCH_REFUSAL_OWNER} holds the legacy table`).toBe(true);
    const main = SCANNED.text(join(SRC_ROOT, 'main.ts'));
    expect(main, 'src/main.ts installs the named wording (stores/versionMismatch.ts)').toMatch(/^installVersionMismatchWording\(\);$/m);
  });

  it('keeps the refusal when a call for an older computer cannot do the action', () => {
    // A fallback that threw its own Error would drop the computer's name,
    // the remedy, and the code and cause a copied report lists. orOlder
    // cannot refuse other throws itself: a fallback's own calls fail for
    // their own reasons (ThreadsPartlyDeleted).
    const bindings = SCANNED.text(join(SRC_ROOT, BINDINGS_WRAPPER));
    expect(bindings, `${BINDINGS_WRAPPER} throws the refusal, with withNote for what the feature adds`).not.toMatch(/throw new Error\(/);
  });

  it('keeps every send on the builder that mints its idempotency id', () => {
    const offenders = new Map<string, string[]>();
    let senders = 0;
    for (const source of sources) {
      if (source.path === BINDINGS_WRAPPER || source.path === SEND_OPTIONS_OWNER) continue;
      const reached = new Set<string>();
      let buildsOptions = false;
      for (const parsed of source.imports) {
        for (const name of parsed.names) {
          if ((SEND_RPCS as readonly string[]).includes(name)) reached.add(name);
          if (name === SEND_OPTIONS_BUILDER) buildsOptions = true;
        }
      }
      if (reached.size === 0) continue;
      senders += 1;
      if (buildsOptions || source.namesSendOptionsType) continue;
      offenders.set(source.path, [
        `calls ${[...reached].sort().join(' / ')} while neither importing ${SEND_OPTIONS_BUILDER}`
        + ' nor taking built OutgoingSendOptions, so its options carry no send id and a retried'
        + ' frame starts a second turn',
      ]);
    }
    // A rule scanning for RPCs nobody imports any more finds nothing and says
    // so cheerfully; the renames these names could take are exactly the case.
    expect(
      senders,
      `no module imports ${SEND_RPCS.join(' or ')}; this rule is scanning for nothing`,
    ).toBeGreaterThan(0);
    expectAllowlistExact(
      offenders,
      SEND_OPTIONS_ALLOWLIST,
      'New violations.',
      `Build the options with ${SEND_OPTIONS_BUILDER} from ${SEND_OPTIONS_OWNER}. It is the`
      + ' one place a sendId is minted, and one call is one send.',
    );
  });

  it('keeps scroll content free of authored compositor state', () => {
    const actual = new Set<string>();
    for (const file of SCANNED.files(/\.(css|svelte|ts)$/)) {
      const path = repoPath(file);
      const inScrollPresentationTree =
        path === 'app.css'
        || SCROLL_PRESENTATION_PREFIXES.some((prefix) => path.startsWith(prefix));
      for (const finding of findCompositorSourceFindings(SCANNED.text(file))) {
        const isPromotion = /^will(?:-change|Change)/.test(finding.kind);
        if (!isPromotion && !inScrollPresentationTree) continue;
        actual.add(`${path}|${finding.kind}|${finding.source}`);
      }
    }
    const sortedActual = [...actual].sort();
    expect(
      sortedActual,
      'Authored scroll-presentation state changed. New entries need a bounded visual owner; removed entries must leave this shrink-only inventory.',
    ).toEqual([...AUTHORIZED_SCROLL_PRESENTATION_STATE].sort());

    const offenders = new Map<string, string[]>();
    for (const [file, text] of Object.entries(PERFPROBE_SOURCES)) {
      if (!text.includes('scroll-composited-content')) continue;
      const path = file.replace(/^(\.\.\/)+/, '');
      offenders.set(path, ['queries the deleted composited-content class']);
    }
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Drive visible motion through scrollTop. Do not add compositor state to controller surfaces or restore probes that query the deleted layer class.',
    );
  });

  // The native-menu guard runs after every app handler (window, bubble
  // phase; utils/browserHistoryGuard.ts). A `contextmenu` handler that stops
  // propagation hides the event from it, so it must claim the event first.
  it('keeps every contextmenu that stops propagation claimed first', () => {
    const offenders = new Map<string, string[]>();
    let handlersSeen = 0;
    for (const file of SCANNED.files(SOURCE_EXTENSIONS)) {
      const text = SCANNED.text(file);
      if (text.includes('contextmenu')) handlersSeen += 1;
      const findings = findContextMenuPropagationFindings(file, text);
      if (findings.length > 0) offenders.set(repoPath(file), findings);
    }
    expect(handlersSeen, 'the scan found no contextmenu handlers; it would pass vacuously').toBeGreaterThan(5);
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Call event.preventDefault() (the handler opened its own menu) or suppressNativeContextMenu(event)'
      + ' from utils/browserHistoryGuard.ts before stopping propagation, and declare the handler in the'
      + ' same file so its body can be checked.',
    );
  });

  // A local Enter handler owns plain Enter and Shift+Enter only
  // (utils/localEnter.ts): the global dispatcher declines a prevented event,
  // so a menu item, row or field that takes every Enter swallows the app's
  // Enter chords (alt+shift+enter, pane.togglePromoted). An Enter key is
  // read through `isLocalEnter`, or `isEnterKey` where the handler checks
  // the modifiers itself; a `case 'Enter':` applies `isLocalEnter` on its
  // first line.
  it('reads the Enter key only through the local Enter rule', () => {
    const offenders = new Map<string, string[]>();
    let enterReads = 0;
    for (const file of SCANNED.files(SOURCE_EXTENSIONS)) {
      if (repoPath(file) === 'lib/utils/localEnter.ts') continue;
      const lines = SCANNED.text(file).split('\n');
      if (lines.some((line) => /\bis(?:Local)?Enter(?:Key)?\(/.test(line))) enterReads += 1;
      const findings: string[] = [];
      lines.forEach((line, index) => {
        if (/\bkey\s*[!=]==?\s*['"]Enter['"]/.test(line)) {
          findings.push(`line ${index + 1}: compares the key with 'Enter'`);
        } else if (/\bcase\s+['"]Enter['"]\s*:/.test(line)) {
          const next = lines.slice(index + 1).find((candidate) => candidate.trim() !== '' && !/^\s*case\b/.test(candidate)) ?? '';
          if (!next.includes('isLocalEnter(')) findings.push(`line ${index + 1}: a case 'Enter' that does not apply isLocalEnter first`);
        }
      });
      if (findings.length > 0) offenders.set(repoPath(file), findings);
    }
    expect(enterReads, 'the scan found no Enter handlers; it would pass vacuously').toBeGreaterThan(20);
    expectAllowlistExact(
      offenders,
      {
        'lib/components/composer/composerKeyboard.ts':
          'popoverNav reduces key names; popoverYieldsKey applies the rule before any caller consults it.',
      },
      'New violations.',
      'Read the key with isLocalEnter(event) from utils/localEnter.ts, or isEnterKey(event) where the handler'
      + ' checks the modifiers itself (a ctrl/meta+Enter chord).',
    );
  });

  // A workspace lets the parser claim `![x](/abs/path.png)`, and the bytes
  // are read from the THREAD's computer (StreamdownImageHost's `backend`,
  // from ChatMarkdown's `threadId`). A call site that names a workspace
  // without the thread would read every such image from the page's own
  // backend, which on a paired browser is the wrong machine.
  it('names the thread beside every workspace a ChatMarkdown is given', () => {
    const offenders = new Map<string, string[]>();
    let withWorkspace = 0;
    for (const file of SCANNED.files(/\.svelte$/)) {
      const text = SCANNED.text(file);
      const findings: string[] = [];
      for (const match of text.matchAll(/<ChatMarkdown\b[^>]*?(?:\/>|>)/gs)) {
        const tag = match[0];
        if (!/\bworkspacePath\b/.test(tag)) continue;
        withWorkspace += 1;
        if (/\bthreadId\b/.test(tag)) continue;
        const line = text.slice(0, match.index).split('\n').length;
        findings.push(`line ${line}: <ChatMarkdown> passes workspacePath without threadId`);
      }
      if (findings.length > 0) offenders.set(repoPath(file), findings);
    }
    expect(withWorkspace, 'the scan found no workspace-bearing ChatMarkdown; it would pass vacuously').toBeGreaterThan(5);
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Pass threadId (the item or pane thread the markdown belongs to) beside workspacePath.',
    );
  });

  // A control a grant switches off says why through `grantBlockedTitle`
  // (transport/scopeRefusal.ts), given the three-way `ScopeGrant`, so a
  // computer that has not answered in this window is never described as
  // having refused this device. The parameter type keeps a boolean out; a
  // type cannot keep out a string literal written into a title, nor a call
  // made only on one side of a two-way check, so this scan does.
  it('says why a grant switches a control off only through grantBlockedTitle', () => {
    const owner = 'lib/transport/scopeRefusal.ts';
    const offenders = new Map<string, string[]>();
    let ownerCalls = 0;
    for (const file of SCANNED.files(SOURCE_EXTENSIONS)) {
      if (repoPath(file) === owner) continue;
      const findings: string[] = [];
      SCANNED.text(file).split('\n').forEach((line, index) => {
        if (/granted to this device/i.test(line)) findings.push(`line ${index + 1}: writes the refusal sentence itself`);
        for (const call of line.matchAll(/(\?\s*)?\bgrantBlockedTitle\(\s*(['"`])?/g)) {
          ownerCalls += 1;
          if (call[2]) findings.push(`line ${index + 1}: passes grantBlockedTitle a literal grant`);
          else if (call[1]) findings.push(`line ${index + 1}: calls grantBlockedTitle on one side of a two-way check`);
        }
      });
      if (findings.length > 0) offenders.set(repoPath(file), findings);
    }
    expect(ownerCalls, 'the scan found no grantBlockedTitle calls; it would pass vacuously').toBeGreaterThan(30);
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Derive the ScopeGrant (scopeGrant, threadScopeGrant, projectScopeGrant, workflowItemScopeGrant) and'
      + ' set the title to grantBlockedTitle(grant) ?? <the title the control has when granted>.',
    );
  });

  // A component holds only the grant (`scopeGrant` and the entity
  // `…ScopeGrant`), never the yes/no answer, so a control it switches off
  // has its reason in hand: the yes/no answer is false for a refusal, for a
  // computer that has not answered, and for an item no computer is known to
  // hold, and a title built from it says the wrong one. Stores keep the
  // yes/no helpers for gates that show nothing. TypeScript cannot keep an
  // export out of one directory, so this scan does.
  it('keeps the yes/no grant helpers out of components', () => {
    const offenders = new Map<string, string[]>();
    let grantReads = 0;
    for (const file of SCANNED.files(SOURCE_EXTENSIONS)) {
      if (!repoPath(file).startsWith('lib/components/')) continue;
      const findings: string[] = [];
      SCANNED.text(file).split('\n').forEach((line, index) => {
        if (/ScopeGrant\(|\bscopeGrant\(/.test(line)) grantReads += 1;
        if (/\b(?:threadHasScope|projectHasScope)\b/.test(line)) {
          findings.push(`line ${index + 1}: uses an entity's yes/no grant helper`);
        } else if (/\bhasScope\(\s*(?!['"]host['"])/.test(line)) {
          findings.push(`line ${index + 1}: asks hasScope for a grant`);
        }
      });
      if (findings.length > 0) offenders.set(repoPath(file), findings);
    }
    expect(grantReads, 'the scan found no grant reads in components; it would pass vacuously').toBeGreaterThan(30);
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Read the grant (scopeGrant, threadScopeGrant, projectScopeGrant, workflowItemScopeGrant). A control it'
      + " switches off takes grantBlockedTitle(grant) as its reason; a gate that shows nothing compares it with 'granted'.",
    );
  });
});
