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
import { describe, it } from 'vitest';
import {
  SRC_ROOT,
  expectAllowlistExact,
  repoPath,
  scannedSources,
  walkSources,
} from '../test/sourceScan';

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

const ENTITY_OWNED_BINDINGS: Record<string, EntityOwnedBinding> = {
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
    'background-task events are used only as debounced refetch triggers for this thread; no payload state is kept',
  'lib/components/composer/workspace/EnvPicker.svelte':
    'turn/background activity re-fetches the OPEN popover\'s worktree rows; the subscription lives and dies with the popover',
  'lib/components/takecontrol/TakeControlPane.svelte':
    'provider:session_died is this pane\'s own close signal — pane lifecycle, not shared state',
  'lib/components/takecontrol/TakeControlTerminal.svelte':
    'provider:terminal_output is a byte stream written straight into this pane\'s xterm; buffering it in a store would duplicate the terminal\'s scrollback',
};

// These are the content surfaces the scroll controller can move. Authored
// promotion or transform state creates a second paint position outside the
// scrollTop chokepoint and can leave WebView2 presenting stale layer pixels.
const SCROLL_CONTENT_SURFACES = [
  'app.css',
  'lib/components/chat/MessageTimeline.svelte',
  'lib/components/chat/ActivityRun.svelte',
  'lib/components/discussion/ChannelView.svelte',
  'lib/components/virtual/TimelineVirtualizer.svelte',
  'lib/utils/scroll/chokepoint.ts',
] as const;
const AUTHORED_SCROLL_COMPOSITOR_STATE =
  /scroll-composited-content|will-change\s*:|style:(?:transform|translate|rotate)|setProperty\(\s*['"](?:translate|rotate)['"]/;
const PERFPROBE_ROOT = resolve(SRC_ROOT, '..', '..', 'scripts', 'perfprobe');

// ---------------------------------------------------------------------------

interface ParsedImport {
  /** Module specifier as written. */
  readonly specifier: string;
  /** Named (non-type) bindings pulled in. */
  readonly names: readonly string[];
  /** `import * as x` or `import('…')` — grants the module's whole surface. */
  readonly wholeModule: boolean;
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
    imports.push({ specifier: match[3]!, ...parseClause(match[1]!) });
  }
  for (const match of source.matchAll(SIDE_EFFECT_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: false });
  }
  for (const match of source.matchAll(DYNAMIC_IMPORT)) {
    imports.push({ specifier: match[2]!, names: [], wholeModule: true });
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
      `Call openInEditor() from ${OPEN_IN_EDITOR_OWNER} so view-only sessions cannot reach the LocalOnly RPC.`,
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

  it('keeps scroll content free of authored compositor state', () => {
    const offenders = new Map<string, string[]>();
    for (const path of SCROLL_CONTENT_SURFACES) {
      const text = readFileSync(join(SRC_ROOT, path), 'utf8');
      if (AUTHORED_SCROLL_COMPOSITOR_STATE.test(text)) {
        offenders.set(path, ['authors content promotion or transform state']);
      }
    }
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
