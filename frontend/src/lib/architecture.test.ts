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

import { readFileSync } from 'node:fs';
import { dirname, join, resolve, sep } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  SRC_ROOT,
  expectAllowlistExact,
  repoPath,
  scannedSources,
  walkSources,
} from '../test/sourceScan';
import { findCompositorSourceFindings } from '../test/compositorSourceScan';

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
const PR_REVIEW_CI = 'lib/stores/prReviewCI.svelte.ts';
const PR_REVIEW_CONFLICTS = 'lib/stores/prReviewConflicts.svelte.ts';
const MCP_SERVERS_STORE = 'lib/stores/mcpServers.svelte.ts';
const CHAT_BAR_FAVORITES_STORE = 'lib/stores/chatBarFavorites.svelte.ts';
const WORKFLOW_RUN_MAP_STORE = 'lib/stores/workflowRunMap.svelte.ts';
const APPEARANCE_STORE = 'lib/stores/appearance.svelte.ts';
const EDITORS_STORE = 'lib/stores/editors.svelte.ts';
const BROWSER_COMPANION_STORE = 'lib/stores/browserCompanion.svelte.ts';
const PROVIDER_ACCOUNTS_STORE = 'lib/stores/providerAccounts.svelte.ts';
const SYSTEMS_STORE = 'lib/stores/systems.svelte.ts';
const SERVICE_UPDATE_STORE = 'lib/stores/serviceUpdate.svelte.ts';
const DEV_SERVERS_STORE = 'lib/stores/devServers.svelte.ts';
const THREADS_STORE = 'lib/stores/threads.svelte.ts';

const ENTITY_OWNED_BINDINGS: Record<string, EntityOwnedBinding> = {
  BrowserCompanionPaneAttach: owned(BROWSER_COMPANION_STORE, 'attachBrowserCompanion()'),
  BrowserCompanionPaneDetach: owned(BROWSER_COMPANION_STORE, 'the attachment release()'),
  BrowserCompanionPaneRect: owned(BROWSER_COMPANION_STORE, 'reportBrowserPaneRect()'),
  BrowserCompanionDo: owned(BROWSER_COMPANION_STORE, 'browserCompanionAct()'),
  BrowserCompanionThreadState: owned(BROWSER_COMPANION_STORE, 'hydrateBrowserCompanionState()'),
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
  GetPRCIJobs: owned(PR_REVIEW_CI, 'loadPRCIJobs()'),
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
  SetChatBarFavorite: owned(CHAT_BAR_FAVORITES_STORE, 'setChatBarFavorite()'),
  WorkflowGetRunMap: owned(WORKFLOW_RUN_MAP_STORE, 'the run-map entity source'),
  // Appearance is one app-global value over one directory, so its store is a
  // plain module store rather than an entityStore — but the ownership rule is
  // the same one, and it has teeth here: a settings section that called
  // SetAppearance itself would hold a selection the applier never sees, and a
  // surface that called SetWindowBackgroundColor outside the store would race
  // the cache the next launch reads.
  GetThemeFiles: owned(APPEARANCE_STORE, 'loadAppearance()'),
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
  RenameBackend: owned(SYSTEMS_STORE, 'renameSystem()'),
  // Updating a supervised machine is one status box per backend fed by two
  // channels and one request. A card calling RequestServiceUpdate itself
  // would start a flow the box the card renders from never marked as its
  // own, and a second reader of GetServiceUpdateStatus would hold a status
  // the frames never converge.
  GetServiceUpdateStatus: owned(SERVICE_UPDATE_STORE, 'loadMachineUpdate()'),
  ListServiceReleases: owned(SERVICE_UPDATE_STORE, 'loadServiceReleases()'),
  RequestServiceUpdate: owned(SERVICE_UPDATE_STORE, 'requestServiceUpdate()'),
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
};

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
    'background-task events are used only as rate-bounded refetch triggers for this thread; no payload state is kept',
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
// additionally reviewed across the whole chat/discussion/virtual/scroll tree,
// not a list of today's plane files: adding a new row component must not
// create an enforcement blind spot.
const SCROLL_PRESENTATION_PREFIXES = [
  'lib/components/chat/',
  'lib/components/discussion/',
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
  'lib/components/chat/CompactionDivider.svelte|Tailwind transform utility|class:rotate-90={expanded}',
  'lib/components/chat/DiagramModal.svelte|Svelte transform style directive|style:transform={transform}',
  'lib/components/chat/ExpandedImageDialog.svelte|Tailwind transform utility|class="absolute left-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"',
  'lib/components/chat/ExpandedImageDialog.svelte|Tailwind transform utility|class="absolute right-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"',
  'lib/components/chat/MessageNavRail.svelte|Svelte transform style directive|style:transform={tickStyleTransform(i)}',
  "lib/components/chat/MessageNavRail.svelte|Svelte transform style directive|style:transform={previewCard?.translateY ?? 'none'}",
  'lib/components/chat/MessageNavRail.svelte|Tailwind transform utility|class="absolute h-[3px] w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-accent/75"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|Tailwind transform utility|class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 -translate-y-px border-x-[6px] border-t-[6px] border-x-transparent border-t-surface-2"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|Tailwind transform utility|class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 border-x-[7px] border-t-[7px] border-x-transparent border-t-border"',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|transform declaration or keyframe|style={`top: ${pendingSelection.composerTop}px; left: ${pendingSelection.composerLeft}px; transform: translate(-50%, 0);`}',
  'lib/components/chat/ProposedPlanReviewSurface.svelte|transform declaration or keyframe|style={`top: ${pendingSelection.triggerTop}px; left: ${pendingSelection.triggerLeft}px; transform: translate(-50%, -100%);`}',
  "lib/components/chat/ScrollToBottomButton.svelte|Tailwind transform utility|'hover:bg-surface-2/80 hover:text-text-primary hover:scale-105 active:scale-95',",
  "lib/components/chat/TailClampedText.svelte|transform declaration or keyframe|[{ transform: `translateY(${decision.startPx}px)` }, { transform: 'translateY(0px)' }],",
  "lib/components/chat/ThreadTitleRegenerateButton.svelte|continuous spin animation|<Icon icon={RefreshCw} size={12} strokeWidth={2} class={pending ? 'animate-spin' : ''} />",
  'lib/components/chat/TranscriptDisclosureHeader.svelte|Tailwind transform utility|class:rotate-90={expandable && expanded}',
  "lib/components/chat/messageNavRailSync.ts|DOM transform property assignment|strip.style.transform = clip > 0 ? `translateY(${-clip}px)` : '';",
] as const;
const PERFPROBE_ROOT = resolve(SRC_ROOT, '..', '..', 'scripts', 'perfprobe');

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
    imports.push({ specifier: match[3]!, ...parseClause(match[1]!), dynamic: false });
  }
  for (const match of source.matchAll(SIDE_EFFECT_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: false, dynamic: false });
  }
  for (const match of source.matchAll(DYNAMIC_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: true, dynamic: true });
  }
  return imports;
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

// A call, not the words: the comments in lib/transport/ discuss `Events.On`
// by name and must not read as subscriptions.
const EVENTS_ON_CALL = /\bEvents\s*\.\s*On\s*\(/;

describe('architecture', () => {
  // Each file's text is read, reduced to the two facts the rules need, and
  // dropped — holding ~800 whole sources for the lifetime of the suite bought
  // nothing but resident bytes.
  const sources = scannedSources(SOURCE_EXTENSIONS).map((file) => {
    const text = readFileSync(file, 'utf8');
    return {
      file,
      path: repoPath(file),
      imports: parseImports(text),
      callsEventsOn: EVENTS_ON_CALL.test(text),
      callsRandomUUID: RANDOM_UUID_CALL.test(text),
      namesSendOptionsType: SEND_OPTIONS_TYPE.test(text),
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
    for (const file of scannedSources(/\.(css|svelte|ts)$/)) {
      const path = repoPath(file);
      const inScrollPresentationTree =
        path === 'app.css'
        || SCROLL_PRESENTATION_PREFIXES.some((prefix) => path.startsWith(prefix));
      for (const finding of findCompositorSourceFindings(readFileSync(file, 'utf8'))) {
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
    for (const file of walkSources(PERFPROBE_ROOT, /\.mjs$/)) {
      const text = readFileSync(file, 'utf8');
      if (!text.includes('scroll-composited-content')) continue;
      const relativePath = file
        .slice(PERFPROBE_ROOT.length + 1)
        .split(sep)
        .join('/');
      const path = `scripts/perfprobe/${relativePath}`;
      offenders.set(path, ['queries the deleted composited-content class']);
    }
    expectAllowlistExact(
      offenders,
      {},
      'New violations.',
      'Drive visible motion through scrollTop. Do not add compositor state to controller surfaces or restore probes that query the deleted layer class.',
    );
  });
});
