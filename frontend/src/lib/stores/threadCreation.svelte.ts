import { GetThreadDefaults, StartTerminal } from './bindings';
import { getProject, getProjects } from './projects.svelte';
import {
  ensureMainPane,
  ensurePaneInLayout,
  getFocusedPaneOrNull,
  openEmptyPane,
  replaceThreadInPane,
} from './panes.svelte';
import { expandProject } from './sidebar.svelte';
import { prependThread } from './threads.svelte';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import type { DraftPlaceholderDefaults, ThreadPane } from './thread.svelte';
import type { Project, Thread } from '../types/models';

export type DraftMode = 'chat' | 'design';

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

async function loadAndStartDraftPlaceholder(
  pane: ThreadPane,
  project: Project,
  mode: DraftMode,
): Promise<boolean> {
  // Reserve the pane before the RPC. A second "+ New" request or a thread
  // switch must win even if this older defaults response resolves last.
  const request = beginDraftDefaultsRequest(pane);
  let defaults: DraftPlaceholderDefaults | undefined;
  try {
    defaults = await GetThreadDefaults({ projectId: project.id, mode });
  } catch (err) {
    console.warn('GetThreadDefaults failed; using empty placeholder defaults', err);
  }

  if (!draftDefaultsRequestIsCurrent(pane, request)) {
    finishDraftDefaultsRequest(pane, request);
    return false;
  }

  pane.startDraftPlaceholder(project, mode, defaults);
  finishDraftDefaultsRequest(pane, request);
  return true;
}

/**
 * Fetch fresh seed defaults and replace the pane's draft placeholder
 * with one keyed on the new (project, mode). The composer pickers
 * (ProjectPicker, ThreadModePicker) both call this to keep the
 * placeholder's toolbar (model, effort, runtime mode) and workspace
 * strip (current git branch) populated across flips — calling
 * `pane.startDraftPlaceholder` directly drops the seeded values and
 * the toolbar/branch render empty.
 *
 * Defaults-fetch failures are swallowed to a warning (mirrors the
 * shape of openDraftThreadForProject): an empty toolbar is better
 * than failing the flip on a UI gesture.
 */
export async function flipPaneDraftPlaceholder(
  pane: ThreadPane,
  project: Project,
  mode: DraftMode,
): Promise<void> {
  await loadAndStartDraftPlaceholder(pane, project, mode);
}

/**
 * Resolve which project the next draft thread should land in when the
 * source of the request doesn't supply one (e.g. the global Ctrl+N
 * keybinding firing without a focused thread). Prefers the focused
 * pane's current project, then falls back to the most recently active
 * project (ListProjects is sorted server-side by lastActive
 * descending). Mode flows through from the caller — "+ New" defaults
 * to chat; the design palette command passes 'design'. Returns null
 * when no projects exist at all — the caller should surface "add a
 * project first".
 */
export function resolveDraftTargetProject(
  targetPane: ThreadPane | null,
  mode: DraftMode,
): { projectId: string; mode: DraftMode } | null {
  const fromPane = targetPane?.thread?.projectId;
  if (fromPane) {
    return { projectId: fromPane, mode };
  }
  const fallback = getProjects()[0]?.project.id;
  if (!fallback) return null;
  return { projectId: fallback, mode };
}

export interface OpenDraftThreadOptions {
  projectId: string;
  mode: DraftMode;
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
 * Returns the pane when this request opened the placeholder, or null when a
 * newer draft request/navigation superseded it while defaults were loading.
 */
export async function openDraftThreadForProject(
  options: OpenDraftThreadOptions,
): Promise<ThreadPane | null> {
  const { projectId, mode, targetPane, openInNewPane = false } = options;
  expandProject(projectId);
  const project = getProject(projectId)?.project;
  if (!project) {
    throw new Error('Project not found');
  }
  const pane: ThreadPane = openInNewPane
    ? openEmptyPane()
    : (targetPane ?? getFocusedPaneOrNull() ?? ensureMainPane());
  // The placeholder is in-memory only — it doesn't go through
  // openThreadInPane, so we need to make sure the pane is mounted in
  // the layout grid ourselves. openEmptyPane already attaches itself.
  ensurePaneInLayout(pane.paneId);
  // Fetch the same seed values CreateThread would have used (last-used
  // model profile + current git branch) so the placeholder's toolbar
  // and workspace strip don't render "no model / no branch" before
  // materialization. Failure here is tolerable — we still want the
  // placeholder to appear; the user can pick from the toolbar.
  const opened = await loadAndStartDraftPlaceholder(pane, project, mode);
  return opened ? pane : null;
}

export interface OpenTerminalThreadOptions {
  /** Project to root the terminal in. Every live entry point passes one — a
   *  project-less terminal would have no sidebar surface. */
  projectId?: string;
  /** Explicit working directory. Omitted → backend resolves (project root or home). */
  cwd?: string;
}

/**
 * Create a persistent `mode:'terminal'` thread and open it in a fresh pane.
 * Every terminal entry point routes here — the per-project `+terminal`
 * button, the `mod+shift+~` chord, and the ChatHeader ctrl/cmd-click — so a
 * terminal always lands in its own new pane (locked decision: always fresh).
 *
 * `StartTerminal` writes the SQLite row (sentinel provider; `workspacePath` =
 * resolved cwd — project root when `projectId` is set, else home) but does NOT
 * spawn a PTY. The shell is opened by `TerminalSurface.onMount` once the pane
 * mounts, which is why a restored terminal re-spawns a fresh shell in its saved
 * cwd for free. The new row is prepended to the sidebar store (mirroring draft
 * materialization) so it shows immediately rather than only after the next
 * thread-list refresh.
 *
 * Two timing decisions are load-bearing:
 *  - The focus latch is set BEFORE `replaceThreadInPane`. `switchThread` mounts
 *    `TerminalView` synchronously inside its await and `TerminalSurface.onMount`
 *    consumes the latch on that mount — latching after the open is one tick too
 *    late and the new shell never grabs focus.
 *  - `replaceThreadInPane` is called directly rather than `openThreadInPane`.
 *    The thread is brand-new, so the `revealThreadIfOpen` probe can never hit;
 *    skipping it also removes the extra await between minting the empty pane and
 *    `switchThread`, keeping the empty-pane → terminal transition in a single
 *    paint frame (no "pick a project" flash).
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
    thread = await StartTerminal({ projectId, cwd });
  } catch (err) {
    console.error('StartTerminal failed', err);
    addToast('error', `Could not start terminal: ${errString(err)}`);
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
  await replaceThreadInPane(thread, pane, 'committed');
  return pane;
}
