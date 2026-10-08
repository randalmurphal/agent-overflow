import { GetThreadDefaults, StartTerminal } from './bindings';
import { getProject, getProjects } from './projects.svelte';
import {
  ensureMainPane,
  ensurePaneInLayout,
  getFocusedPaneOrNull,
  mountThreadInPane,
  openEmptyPane,
  threadHostPane,
} from './panes.svelte';
import { expandProject } from './sidebar.svelte';
import { prependThread } from './threads.svelte';
import { addErrorToast } from './toast.svelte';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { DraftPlaceholderDefaults, ThreadPane } from './thread.svelte';
import type { Project, Thread } from '../types/models';
import { getThreadGroupById } from './threadGroups.svelte';
import { preferredProjectTarget } from './projectTargets';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { projectBackend } from '../transport/entityIndex';
import { moveDraftProject } from './draftProjectMove';

interface DraftDefaultsRequest {
  token: object;
  switchGeneration: number;
  threadId: string | null;
}

const latestDraftDefaultsRequest = new WeakMap<ThreadPane, object>();

function beginDraftDefaultsRequest(pane: ThreadPane): DraftDefaultsRequest {
  const token = {};
  latestDraftDefaultsRequest.set(pane, token);
  return {
    token,
    switchGeneration: pane.switchGeneration,
    threadId: pane.thread?.id ?? null,
  };
}

function draftDefaultsRequestIsCurrent(
  pane: ThreadPane,
  request: DraftDefaultsRequest,
): boolean {
  return latestDraftDefaultsRequest.get(pane) === request.token
    && pane.switchGeneration === request.switchGeneration
    && (pane.thread?.id ?? null) === request.threadId;
}

function finishDraftDefaultsRequest(
  pane: ThreadPane,
  request: DraftDefaultsRequest,
): void {
  if (latestDraftDefaultsRequest.get(pane) === request.token) {
    latestDraftDefaultsRequest.delete(pane);
  }
}

/**
 * The seed a placeholder can be started with before the backend answers.
 *
 * A pane that already shows a placeholder carries its selection across a
 * project flip, so the toolbar keeps a model, effort and runtime mode while
 * the new project's defaults load instead of blanking and refilling. Branch
 * and workspace are deliberately absent: they described the project being
 * left. Anything else (a live thread, an empty pane) starts from the
 * toolbar's own fallbacks, because a neighbouring thread's model is not a
 * statement about what the next one should be.
 */
function localDraftDefaults(pane: ThreadPane): DraftPlaceholderDefaults | undefined {
  const thread = pane.draftPlaceholder ? pane.thread : null;
  if (!thread) return undefined;
  return {
    provider: thread.provider,
    model: thread.model,
    reasoningEffort: thread.reasoningEffort,
    fastMode: thread.fastMode,
    contextWindow: thread.contextWindow,
    runtimeMode: thread.runtimeMode,
  };
}

/**
 * Open the placeholder NOW, then converge it on the backend's defaults.
 *
 * The placeholder is pure UI state, so nothing about it needs the RPC: during
 * a store stall `GetThreadDefaults` took seconds and "+ New" painted nothing
 * at all. Starting first means the pane is usable immediately and the seed
 * (model, effort, runtime mode, branch) lands when it arrives.
 *
 * The pane is reserved on the same tick as the start, so a second "+ New"
 * request or a thread switch wins even if this older response resolves last.
 * A failed fetch leaves the placeholder open on its fallback defaults — the
 * toolbar pickers resolve their own values — and is reported as a diagnostic
 * rather than a toast, since the user got the surface they asked for.
 *
 * Resolves true when this request still owned the placeholder as the answer
 * landed, false when a newer request or a navigation superseded it.
 */
async function loadAndStartDraftPlaceholder(
  pane: ThreadPane,
  project: Project,
  groupId?: string,
): Promise<boolean> {
  pane.startDraftPlaceholder(
    project,
    'chat',
    localDraftDefaults(pane),
    groupId && getThreadGroupById(groupId) ? groupId : undefined,
  );
  const request = beginDraftDefaultsRequest(pane);
  let defaults: DraftPlaceholderDefaults | undefined;
  try {
    defaults = await withBackendTarget(requireEntityBackend(projectBackend(project.id)),
      () => GetThreadDefaults({ projectId: project.id, mode: 'chat' }));
  } catch (err) {
    reportFrontendDiagnostic('thread defaults fetch failed', errString(err));
  }

  if (!draftDefaultsRequestIsCurrent(pane, request)) {
    finishDraftDefaultsRequest(pane, request);
    return false;
  }

  if (defaults) pane.applyDraftPlaceholderDefaults(defaults);
  finishDraftDefaultsRequest(pane, request);
  return true;
}

/**
 * Change the draft's project, carrying its content and selected settings. A
 * project no computer is known to own is refused and the draft stays put.
 */
export async function switchDraftProject(
  pane: ThreadPane,
  project: Project,
): Promise<boolean> {
  return moveDraftProject(pane, project, async () => {
    const backend = requireEntityBackend(projectBackend(project.id));
    const source = pane.thread!;
    const selected: DraftPlaceholderDefaults = {
      provider: source.provider, model: source.model, reasoningEffort: source.reasoningEffort,
      fastMode: source.fastMode ?? false, contextWindow: source.contextWindow, runtimeMode: source.runtimeMode,
      autoCompactStandardPercent: source.autoCompactStandardPercent,
      autoCompactExtendedPercent: source.autoCompactExtendedPercent,
    };
    pane.startDraftPlaceholder(project, source.mode === 'plan' ? 'plan' : 'chat', selected);
    const request = beginDraftDefaultsRequest(pane);
    try {
      const defaults = await withBackendTarget(backend,
        () => GetThreadDefaults({ projectId: project.id, mode: source.mode }));
      if (!draftDefaultsRequestIsCurrent(pane, request)) return false;
      pane.applyDraftPlaceholderDefaults({ ...defaults, ...(source.model ? selected : {}) });
      return true;
    } finally { finishDraftDefaultsRequest(pane, request); }
  });
}

/**
 * Resolve which project the next draft thread should land in when the
 * source of the request doesn't supply one (e.g. the global Ctrl+N
 * keybinding firing without a focused thread). Prefers the focused
 * pane's current project, then falls back to the most recently active
 * project (ListProjects is sorted server-side by lastActive
 * descending). Returns null when no projects exist at all — the caller should surface "add a
 * project first".
 */
export function resolveDraftTargetProject(
  targetPane: ThreadPane | null,
): { projectId: string } | null {
  const fromPane = targetPane?.thread?.projectId;
  if (fromPane) {
    return { projectId: fromPane };
  }
  const fallback = getProjects()[0]?.project.id;
  if (!fallback) return null;
  return { projectId: fallback };
}

export interface OpenDraftThreadOptions {
  projectId: string;
  groupId?: string;
  targetPane?: ThreadPane | null;
  openInNewPane?: boolean;
}

/**
 * Open a fresh in-pane draft placeholder for a project. The placeholder is
 * a pure UI state — no SQLite row is written. The first composer input
 * (typed text, paste, attachment upload) or toolbar action calls
 * `pane.ensureMaterializedThread()`, which creates the backend row,
 * prepends it to the sidebar with `isDraft=true`, and points the
 * composer-draft store at the new id. "+ New" repeated without any
 * action simply replaces the prior placeholder, so the user can spin up
 * and discard threads freely.
 *
 * The placeholder appears on the calling tick; nothing about it waits on the
 * backend. The returned promise settles when the seed defaults land, so the
 * result still reports ownership: the pane when this request still owned the
 * placeholder at that moment, null when a newer draft request or a
 * navigation superseded it in between.
 */
export async function openDraftThreadForProject(
  options: OpenDraftThreadOptions,
): Promise<ThreadPane | null> {
  const { projectId, groupId, targetPane, openInNewPane = false } = options;
  expandProject(projectId);
  const source = getProject(projectId)?.project;
  if (!source) {
    throw new Error('Project not found');
  }
  if (groupId && getThreadGroupById(groupId)?.projectId !== projectId) {
    throw new Error('That group no longer exists in this project');
  }
  // A group fixes the computer and project; repository preferences cannot redirect it.
  const project = groupId ? source : preferredProjectTarget(source);
  const pane: ThreadPane = openInNewPane
    ? openEmptyPane()
    : threadHostPane(targetPane ?? getFocusedPaneOrNull() ?? ensureMainPane());
  // The placeholder is in-memory only — it doesn't go through
  // openThreadInPane, so we need to make sure the pane is mounted in
  // the layout grid ourselves. openEmptyPane already attaches itself.
  ensurePaneInLayout(pane.paneId);
  // Opens the placeholder, then applies the same seed values CreateThread
  // would have used (last-used model profile + current git branch) so its
  // toolbar and workspace strip converge on the real model and branch.
  const opened = await loadAndStartDraftPlaceholder(pane, project, groupId);
  return opened ? pane : null;
}

export interface OpenTerminalThreadOptions {
  /** Project to root the terminal in; absent beside a thread that has none. */
  projectId?: string;
  /** Explicit working directory. Omitted → backend resolves (project root or home). */
  cwd?: string;
}

/**
 * Create a `mode:'terminal'` thread and open it in a fresh pane.
 * Every terminal entry point routes here — the per-project `+terminal`
 * button, the `mod+shift+~` chord, and the ChatHeader ctrl/cmd-click — so a
 * terminal always lands in its own new pane (locked decision: always fresh).
 *
 * `StartTerminal` writes the SQLite row (sentinel provider; `workspacePath` =
 * resolved cwd — project root when `projectId` is set, else home) but does NOT
 * spawn a PTY. The first shell is opened by `TerminalSurface.onMount` once the
 * pane mounts. The thread lives as long as its shells: when the last one ends,
 * or the backend restarts, the backend deletes it and every client drops it
 * (internal/app/app_terminal_threads.go). The new row is prepended to the
 * sidebar store (mirroring draft materialization) so it shows immediately
 * rather than only after the next thread-list refresh.
 *
 * Two timing decisions are load-bearing:
 *  - The focus latch is set BEFORE `mountThreadInPane`. `switchThread` mounts
 *    `TerminalView` synchronously inside its await and `TerminalSurface.onMount`
 *    consumes the latch on that mount — latching after the open is one tick too
 *    late and the new shell never grabs focus.
 *  - The pane is passed explicitly. `mountThreadInPane`'s already-open probe is
 *    synchronous, so it costs no await here (the thread is brand-new and can
 *    never hit it anyway) and the empty-pane → terminal transition stays in a
 *    single paint frame — no "pick a project" flash.
 *
 * Returns the opened pane, or `null` when `StartTerminal` fails — the failure is
 * surfaced as an error toast rather than an unhandled rejection, since the user
 * clicked expecting a terminal.
 */
export async function openTerminalThread(
  options: OpenTerminalThreadOptions = {},
): Promise<ThreadPane | null> {
  const { projectId, cwd } = options;
  let thread: Thread;
  try {
    // A project names its computer. Without one, the terminal opens beside the
    // focused pane's thread, which is where StartTerminal's `selected` route goes.
    const start = () => StartTerminal({ projectId, cwd });
    thread = await (projectId ? withBackendTarget(requireEntityBackend(projectBackend(projectId)), start) : start());
  } catch (err) {
    console.error('StartTerminal failed', err);
    addErrorToast(`Could not start terminal: ${errString(err)}`, err);
    return null;
  }
  // Reveal where the new row will land (the possibly-collapsed project) so
  // the create isn't invisible.
  if (projectId) expandProject(projectId);
  // Surface the new terminal in the sidebar immediately (mirrors how draft
  // materialization prepends), instead of waiting for a thread-list refresh.
  prependThread(thread);
  const pane = openEmptyPane();
  pane.requestTerminalFocus();
  await mountThreadInPane(thread, pane, 'committed');
  return pane;
}
