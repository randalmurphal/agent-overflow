// Which backend the `selected` route sends a call to.
//
// Most RPCs resolve their backend from an entity they already name — a
// thread id, a project id, a workflow item (transport/entityIndex.ts). The
// 38 `selected` methods name none: a creation has no id yet, and several
// take a WORKSPACE PATH (UpdateThreadBranch, GetWorkspaceActivity,
// GetLocalImageData, StartTerminal), which is not an entity at all because
// the same path names a different checkout on every machine.
//
// So the answer is the machine the person is LOOKING AT, in this order:
//
//  1. The focused thread pane's thread, or its project's owner for an
//     unmaterialized draft. A path-argument call issued while
//     a thread is on screen is about THAT thread's checkout — routing it
//     anywhere else would ask one machine about another's directory, and
//     the answer would be plausible rather than empty. This is why the
//     rule is the focused pane's thread and not merely a picker value: the
//     picker is about where the NEXT thing is created, and a running
//     thread's calls must not follow it.
//  2. Otherwise the draft's chosen backend — a placeholder pane staging a
//     thread on another machine, else the app-wide choice.
//  3. Otherwise this frontend's remembered choice, initially home or the first
//     saved computer on a frontend without a local execution host.
//
// The writers are the composer's workspace strip pickers:
// `MachinePicker.svelte` (which machine) and `ProjectPicker.svelte` (a
// project lives on one machine, so choosing it chooses one). Both stage
// the PANE's choice; the machine picker also moves the app-wide one. On a
// single-backend page nothing writes here and the answer is always home.
//
// Per-repository choices belong to projectTargets.ts. This leaf remembers the
// general selection; a focused conversation's actual owner always wins.
//
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { projectBackend, threadBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { initialComputer } from '../transport/runMode';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';

const STORAGE_KEY = 'selected-computer';
const remembered = readFrontendValue(STORAGE_KEY);
const initial = initialComputer() || (typeof remembered === 'string' && remembered.length <= 128 ? remembered : HOME_BACKEND);
let selected = $state<BackendKey>(initial);

/** Boot only: a frontend without a local computer starts on its first saved one.
 * Explicit launch and remembered choices survive outages and removal. */
export function initializeSelectedBackend(computers: readonly { id: BackendKey }[], preferred?: BackendKey): void {
  if (selected === HOME_BACKEND && !computers.some((computer) => computer.id === HOME_BACKEND)) {
    selected = computers.find((computer) => computer.id === preferred)?.id ?? computers[0]?.id ?? HOME_BACKEND;
  }
}
// The focused thread pane's thread, supplied by stores/panes.svelte.
// A function rather than an import, because `panes → thread →
// gitStatusStore → transport` already exists and importing panes from a
// transport-adjacent leaf would close that ring. The pane module arms this
// when IT loads, the same shape as `setGitStatusPaneBridge`, so there is
// no registration order to get right.
let focusedThread: (() => Pick<Thread, 'id' | 'projectId'> | null) | null = null;

/** Arm the focused-pane resolver. Called once, by stores/panes.svelte. */
export function setFocusedThreadResolver(resolve: () => Pick<Thread, 'id' | 'projectId'> | null): void {
  focusedThread = resolve;
}
// Per-pane overrides: a draft placeholder staging a thread on another
// machine. Keyed by pane id, dropped when the pane closes
// (`stores/panes.svelte.ts`'s `destroyPane`). A plain Map, not a rune: it
// is read on the RPC path and written by a picker, and nothing renders
// from it in this wave.
const byPane = new Map<string, BackendKey>();
// Which pane's override to prefer, as a RESOLVER rather than a value.
//
// The first shipped form was a setter somebody had to call on every focus
// change, and nobody ever did: `activePane` stayed null for the life of
// the app, so every `setPaneBackend` write was dead and the project
// picker's machine choice reached routing through nothing at all. A pull
// cannot be forgotten, which is the same argument `focusedThread` above
// makes and the same shape `setGitStatusPaneBridge` uses.
let activePaneId: (() => string | null) | null = null;

/**
 * The backend a `selected` call goes to. See the order at the top.
 *
 * Reactive when read from a `$derived`, so 7c's picker renders from the
 * same answer routing uses.
 */
export function selectedBackend(): BackendKey {
  const thread = focusedThread?.() ?? null;
  if (thread) {
    // A placeholder has no indexed thread yet, but its project already
    // identifies the execution host. Never let an old selection win it.
    const owner = threadBackend(thread.id) ?? (thread.projectId ? projectBackend(thread.projectId) : undefined);
    if (owner !== undefined) return owner;
  }
  const paneId = activePaneId?.() ?? null;
  const override = paneId === null ? undefined : byPane.get(paneId);
  return override ?? selected;
}

// A remembered but removed computer stays the routing target until the user
// chooses another. Dispatch rejects an absent target; falling back to HOME
// could execute a perfectly valid command against the wrong filesystem.

/** Set the app-wide choice. The picker's write. */
export function setSelectedBackend(backendId: BackendKey): void {
  selected = backendId;
  writeFrontendValue(STORAGE_KEY, backendId);
}

/** Stage a pane's own choice — a draft placeholder's machine. */
export function setPaneBackend(paneId: string, backendId: BackendKey | null): void {
  if (backendId === null) byPane.delete(paneId);
  else byPane.set(paneId, backendId);
}

/**
 * Arm the resolver for "which pane's staged machine counts right now".
 * Called once, by stores/panes.svelte, beside the focused-thread resolver.
 */
export function setActiveBackendPaneResolver(resolve: () => string | null): void {
  activePaneId = resolve;
}

/** Test seam: back to the single-backend answer. */
export function __resetSelectedBackendForTest(): void {
  selected = HOME_BACKEND;
  byPane.clear();
  // Neither resolver is cleared. Both are module wiring armed by
  // stores/panes.svelte at ITS load, not per-test state, and clearing them
  // would silently unarm the rules for every later test in the run.
}
