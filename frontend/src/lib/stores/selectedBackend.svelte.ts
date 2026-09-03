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
//  1. The focused thread pane's thread. A path-argument call issued while
//     a thread is on screen is about THAT thread's checkout — routing it
//     anywhere else would ask one machine about another's directory, and
//     the answer would be plausible rather than empty. This is why the
//     rule is the focused pane's thread and not merely a picker value: the
//     picker is about where the NEXT thing is created, and a running
//     thread's calls must not follow it.
//  2. Otherwise the draft's chosen backend — a placeholder pane staging a
//     thread on another machine, else the app-wide choice.
//  3. Otherwise home.
//
// The writers are the composer's workspace strip pickers:
// `MachinePicker.svelte` (which machine) and `ProjectPicker.svelte` (a
// project lives on one machine, so choosing it chooses one). Both stage
// the PANE's choice; the machine picker also moves the app-wide one. On a
// single-backend page nothing writes here and the answer is always home.
//
// The primitive is the single current choice plus the per-pane override a
// draft placeholder carries, because those are what routing needs. A
// per-project memory ("sticky last-used per project", §10) only has
// something to remember once a project spans machines, which is wave 7d's
// merged entry; it belongs with the picker when that lands.
//
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { backendById } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';

let selected = $state<BackendKey>(HOME_BACKEND);
// The focused thread pane's thread id, supplied by stores/panes.svelte.
// A function rather than an import, because `panes → thread →
// gitStatusStore → transport` already exists and importing panes from a
// transport-adjacent leaf would close that ring. The pane module arms this
// when IT loads, the same shape as `setGitStatusPaneBridge`, so there is
// no registration order to get right.
let focusedThreadId: (() => string | null) | null = null;

/** Arm the focused-pane resolver. Called once, by stores/panes.svelte. */
export function setFocusedThreadResolver(resolve: () => string | null): void {
  focusedThreadId = resolve;
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
// cannot be forgotten, which is the same argument `focusedThreadId` above
// makes and the same shape `setGitStatusPaneBridge` uses.
let activePaneId: (() => string | null) | null = null;

/**
 * The backend a `selected` call goes to. See the order at the top.
 *
 * Reactive when read from a `$derived`, so 7c's picker renders from the
 * same answer routing uses.
 */
export function selectedBackend(): BackendKey {
  const threadId = focusedThreadId?.() ?? null;
  if (threadId !== null && threadId !== '') {
    const owner = threadBackend(threadId);
    if (owner !== undefined) return live(owner);
  }
  const paneId = activePaneId?.() ?? null;
  const override = paneId === null ? undefined : byPane.get(paneId);
  return live(override ?? selected);
}

// A backend that has since detached answers HOME rather than a dead
// handle: an unreachable target must fail visibly at the picker (spec §10,
// "never silent failover"), and a route resolution is not where that
// decision gets made.
function live(choice: BackendKey): BackendKey {
  if (choice === HOME_BACKEND) return HOME_BACKEND;
  return backendById(choice) === undefined ? HOME_BACKEND : choice;
}

/** Set the app-wide choice. The picker's write. */
export function setSelectedBackend(backendId: BackendKey): void {
  selected = backendId;
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
