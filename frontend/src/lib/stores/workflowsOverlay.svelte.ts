// Workflows overlay navigation (UI-SPEC §2). The overlay is a full-surface
// layer rendered as a SIBLING of the pane host — the pane tree stays mounted
// underneath, so opening and closing rebuild nothing. This store owns the only
// state that survives a close: the stack, the project filter, and the sweep
// cursor.
//
// Two levels plus a terminal one: home › run detail, and all-clear (reachable
// only by finishing the sweep, §4.4). There is no third navigation depth — a
// workflow's runs expand in place on home.
//
// Persistence rides appStorage (the ui_state table), so the stack survives a
// restart, not just a reload. `open` is deliberately NOT persisted: a restart
// starts on the pane tree, and reopening the overlay restores where you were.

import { appStorageGet, appStorageSet } from './appStorage';

const STACK_KEY = 'workflows:overlay';

export type WorkflowsOverlayEntry =
  | { level: 'home' }
  | { level: 'run'; itemId: string }
  | { level: 'all-clear' };

/** Transient dialogs. Esc closes the top one before it pops the stack. */
export type WorkflowsOverlayDialog = 'intake' | 'discard' | null;

interface PersistedOverlayState {
  stack: WorkflowsOverlayEntry[];
  projectFilter: string;
  sweepActive: boolean;
  sweepIndex: number;
}

const HOME: WorkflowsOverlayEntry = { level: 'home' };

function defaults(): PersistedOverlayState {
  return { stack: [HOME], projectFilter: '', sweepActive: false, sweepIndex: -1 };
}

function parseEntry(value: unknown): WorkflowsOverlayEntry | null {
  if (!value || typeof value !== 'object') return null;
  const entry = value as Record<string, unknown>;
  if (entry.level === 'home') return HOME;
  if (entry.level === 'all-clear') return { level: 'all-clear' };
  if (entry.level === 'run' && typeof entry.itemId === 'string' && entry.itemId !== '') {
    return { level: 'run', itemId: entry.itemId };
  }
  return null;
}

export function parsePersistedOverlayState(raw: string | null): PersistedOverlayState {
  if (raw === null) return defaults();
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return defaults();
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return defaults();
  const record = parsed as Record<string, unknown>;
  const entries = Array.isArray(record.stack)
    ? record.stack.map(parseEntry).filter((entry): entry is WorkflowsOverlayEntry => entry !== null)
    : [];
  // Home is always the floor: a persisted stack that lost it (or never had it)
  // is repaired rather than rejected, so a corrupt write never strands the
  // overlay on a level with no way back.
  const stack = entries[0]?.level === 'home' ? entries : [HOME, ...entries.filter((entry) => entry.level !== 'home')];
  const sweepIndex = typeof record.sweepIndex === 'number' && Number.isInteger(record.sweepIndex)
    ? record.sweepIndex
    : -1;
  return {
    stack,
    projectFilter: typeof record.projectFilter === 'string' ? record.projectFilter : '',
    sweepActive: record.sweepActive === true,
    sweepIndex: sweepIndex >= -1 ? sweepIndex : -1,
  };
}

const initial = parsePersistedOverlayState(appStorageGet(STACK_KEY));

let open = $state(false);
let stack = $state<WorkflowsOverlayEntry[]>(initial.stack);
let projectFilter = $state(initial.projectFilter);
let sweepActive = $state(initial.sweepActive);
let sweepIndex = $state(initial.sweepIndex);
let dialog = $state<WorkflowsOverlayDialog>(null);
let armedAction = $state<string | null>(null);

function persist(): void {
  appStorageSet(STACK_KEY, JSON.stringify({ stack, projectFilter, sweepActive, sweepIndex }));
}

/** Level changes reset transient state (§2.2): armed confirms, open dialogs. */
function clearTransient(): void {
  dialog = null;
  armedAction = null;
}

export function isWorkflowsOverlayOpen(): boolean {
  return open;
}

export function getWorkflowsOverlayStack(): readonly WorkflowsOverlayEntry[] {
  return stack;
}

export function getWorkflowsOverlayTop(): WorkflowsOverlayEntry {
  return stack[stack.length - 1] ?? HOME;
}

export function getWorkflowsOverlayRunId(): string {
  const top = getWorkflowsOverlayTop();
  return top.level === 'run' ? top.itemId : '';
}

/**
 * Closes the OTHER full-height layer over the pane strip — settings. Both are
 * siblings of PaneHost with their own focus trap, and two of them up at once
 * has no coherent Esc, so opening this one closes that one.
 *
 * Injected rather than imported to keep the import one-way: the settings store
 * imports this module (for `closeWorkflowsOverlay`) and arms this hook at its
 * own module init, so the wiring is deterministic at load time rather than
 * dependent on anything being registered first. It lands on the store rather
 * than on the `workflows.toggle` command because `openWorkflowsOverlay` is the
 * ONE writer of `open = true`: the chord, the sidebar chip and the
 * OS-notification deep link all funnel through it, so no entry point can forget
 * the exclusion.
 */
let closeSettingsSurface: () => void = () => {};

export function setWorkflowsOverlayExclusion(closeSettings: () => void): void {
  closeSettingsSurface = closeSettings;
}

export function openWorkflowsOverlay(): void {
  closeSettingsSurface();
  open = true;
}

export function closeWorkflowsOverlay(): void {
  open = false;
  clearTransient();
}

export function toggleWorkflowsOverlay(): void {
  if (open) closeWorkflowsOverlay();
  else openWorkflowsOverlay();
}

export function pushWorkflowRunDetail(itemId: string, options: { sweep?: boolean; sweepIndex?: number } = {}): void {
  if (!itemId) return;
  clearTransient();
  const top = getWorkflowsOverlayTop();
  // Sweeping replaces the current run rather than deepening the stack: the
  // sweep is one level with a moving target, not a trail of visited runs.
  stack = top.level === 'run' || top.level === 'all-clear'
    ? [...stack.slice(0, -1), { level: 'run', itemId }]
    : [...stack, { level: 'run', itemId }];
  if (options.sweep !== undefined) sweepActive = options.sweep;
  if (options.sweepIndex !== undefined) sweepIndex = options.sweepIndex;
  persist();
}

export function pushWorkflowAllClear(): void {
  clearTransient();
  const top = getWorkflowsOverlayTop();
  stack = top.level === 'home' ? [...stack, { level: 'all-clear' }] : [...stack.slice(0, -1), { level: 'all-clear' }];
  sweepActive = false;
  sweepIndex = -1;
  persist();
}

/** Back / Esc / Backspace at run depth. Returns false at home (caller closes). */
export function popWorkflowsOverlay(): boolean {
  if (stack.length <= 1) return false;
  clearTransient();
  stack = stack.slice(0, -1);
  sweepActive = false;
  sweepIndex = -1;
  persist();
  return true;
}

export function getWorkflowProjectFilter(): string {
  return projectFilter;
}

export function setWorkflowProjectFilter(projectId: string): void {
  if (projectFilter === projectId) return;
  projectFilter = projectId;
  // The filter narrows the sweep set too (§3.1), so the cursor's anchor is no
  // longer meaningful against the new set.
  sweepIndex = -1;
  persist();
}

export function isWorkflowSweepActive(): boolean {
  return sweepActive;
}

export function getWorkflowSweepIndex(): number {
  return sweepIndex;
}

export function setWorkflowSweepCursor(active: boolean, index: number): void {
  sweepActive = active;
  sweepIndex = index;
  persist();
}

export function getWorkflowsOverlayDialog(): WorkflowsOverlayDialog {
  return dialog;
}

export function setWorkflowsOverlayDialog(next: WorkflowsOverlayDialog): void {
  dialog = next;
  if (next !== null) armedAction = null;
}

export function getWorkflowArmedAction(): string | null {
  return armedAction;
}

export function setWorkflowArmedAction(next: string | null): void {
  armedAction = next;
}

/**
 * Esc precedence (§2.2): disarm a confirm → close a dialog → pop → close the
 * overlay. Returns what it consumed so callers can preventDefault only when
 * something actually happened.
 */
export type WorkflowsEscapeOutcome = 'disarmed' | 'dialog-closed' | 'popped' | 'closed';

export function consumeWorkflowsOverlayEscape(): WorkflowsEscapeOutcome {
  if (armedAction !== null) {
    armedAction = null;
    return 'disarmed';
  }
  if (dialog !== null) {
    dialog = null;
    return 'dialog-closed';
  }
  if (popWorkflowsOverlay()) return 'popped';
  closeWorkflowsOverlay();
  return 'closed';
}

/**
 * Open the overlay at one run's detail, inside the sweep — the OS-notification
 * deep link (§7) and the needs-attention row both land here.
 */
export function openWorkflowRunInOverlay(itemId: string, sweepIndex = -1): void {
  openWorkflowsOverlay();
  pushWorkflowRunDetail(itemId, { sweep: true, sweepIndex });
}

/**
 * Drop restored entries whose target no longer exists (§2.1). Home is always
 * valid, so the stack can never empty. Called once the run cache has hydrated.
 */
export function pruneWorkflowsOverlayStack(runExists: (itemId: string) => boolean): void {
  const pruned: WorkflowsOverlayEntry[] = [];
  for (const entry of stack) {
    if (entry.level === 'run' && !runExists(entry.itemId)) break;
    pruned.push(entry);
  }
  if (pruned.length === stack.length) return;
  stack = pruned.length > 0 ? pruned : [HOME];
  sweepActive = false;
  sweepIndex = -1;
  clearTransient();
  persist();
}

/** Adopt the durable copy once appStorage hydration lands (App.svelte boot). */
export function syncWorkflowsOverlayFromAppStorage(): void {
  const raw = appStorageGet(STACK_KEY);
  if (raw === null) return;
  const parsed = parsePersistedOverlayState(raw);
  stack = parsed.stack;
  projectFilter = parsed.projectFilter;
  sweepActive = parsed.sweepActive;
  sweepIndex = parsed.sweepIndex;
}

// `closeSettingsSurface` is deliberately NOT reset here: it is structural
// wiring armed once by the settings store's module init, not per-test state,
// and clearing it would silently disarm the exclusion for every later test.
export function resetWorkflowsOverlayForTest(): void {
  const fresh = defaults();
  open = false;
  stack = fresh.stack;
  projectFilter = fresh.projectFilter;
  sweepActive = fresh.sweepActive;
  sweepIndex = fresh.sweepIndex;
  dialog = null;
  armedAction = null;
}
