// Workflows navigation (UI-SPEC §2). Workflows is the `workflows` tool pane
// (toolPanes.svelte.ts): "open" means that pane exists in the layout, and the
// pane persists with the layout. This store owns the navigation state that
// survives the pane closing: the stack, the project filter, and the sweep
// cursor.
//
// Two levels plus a terminal one: home › run detail, and all-clear (reachable
// only by finishing the sweep, §4.4). There is no third navigation depth — a
// workflow's runs expand in place on home.
//
// Persistence rides appStorage (the ui_state table), so the stack survives a
// restart, not just a reload, and reopening the pane restores where you were.

import { appStorageGet, appStorageSet } from './appStorage';
import {
  addToolPaneClosedObserver,
  closeToolPane,
  focusedToolKind,
  openTool,
  registerToolDetailProbe,
  toggleTool,
  type OpenToolOptions,
} from './toolPanes.svelte';
import { toolPaneIdFor } from './paneLayout.svelte';

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
  // pane on a level with no way back.
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

/** The RAW focused pane is the workflows pane: its chords are live. */
export function isWorkflowsPaneFocused(): boolean {
  return focusedToolKind() === 'workflows';
}

/**
 * Something above home is open: a run detail or all-clear level, a dialog, or
 * an armed confirm. Escape and the phone's Back consume one of these before
 * they leave the pane (`workflows.escape`, gated on `toolDetailOpen`).
 */
export function isWorkflowsDetailOpen(): boolean {
  return armedAction !== null || dialog !== null || stack.length > 1;
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

/** Open or reveal the workflows pane (toolPanes.svelte#openTool). */
export function openWorkflowsPane(options: OpenToolOptions = {}): void {
  openTool('workflows', options);
}

export function closeWorkflowsPane(): void {
  closeToolPane(toolPaneIdFor('workflows'));
}

/** `workflows.toggle`: open or reveal the pane, or close it when focused. */
export function toggleWorkflowsPane(): void {
  toggleTool('workflows');
}

// A dialog or an armed confirm belongs to the pane that showed it. Whichever
// path takes the pane down (its close button, the chord, a thread opened in
// its place), reopening it must not bring them back.
addToolPaneClosedObserver((kind) => {
  if (kind === 'workflows') clearTransient();
});

registerToolDetailProbe('workflows', isWorkflowsDetailOpen);

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
  touchRunMapExpansion();
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

/** Back / Esc / Backspace at run depth. Returns false at home. */
export function popWorkflowsOverlay(): boolean {
  if (stack.length <= 1) return false;
  clearTransient();
  stack = stack.slice(0, -1);
  sweepActive = false;
  sweepIndex = -1;
  touchRunMapExpansion();
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

// ---------------------------------------------------------------------------
// Run-map expansion (RUN-MAP §8)
// ---------------------------------------------------------------------------

/**
 * Which folded waves the reader opened, and which deep compositions they
 * expanded past the §6 depth rule — keyed by run id so a detail REMOUNT
 * (pane close/reopen, a level round-trip, a sweep step and back) does not
 * silently re-fold what someone opened. The run tree itself carries no such
 * bit; this is view state about a run, which is why it lives with the
 * pane's other navigation state rather than in the map's data store.
 *
 * Deliberately NOT persisted, unlike the stack: the key space is every run
 * that ever existed, and the persisted row is one small blob the whole pane
 * shares. Bounded to the last few runs for the same reason — a session that
 * sweeps two hundred runs must not accumulate two hundred sets. Follow
 * engagement is not here at all: §9.4 makes it per-visit by design.
 */
export interface WorkflowRunMapExpansion {
  /** Folded (terminal) waves the reader expanded, by wave item id. */
  readonly waves: ReadonlySet<string>;
  /** Compositions the reader opened, by called-run item id — folded by default (§3). */
  readonly compositions: ReadonlySet<string>;
  /**
   * Settled fan lanes the reader opened, by BRANCH key. Kept apart from
   * `compositions` because a lane is a unit and not a called run: one lane can
   * hold several called runs, and the click that opens it has to name the lane.
   */
  readonly lanes: ReadonlySet<string>;
}

const RUN_MAP_EXPANSION_LIMIT = 8;

// Replaced wholesale on every write (a handful of ids per run), so plain
// `$state` reactivity covers it without a SvelteMap.
let runMapExpansion = $state(new Map<string, WorkflowRunMapExpansion>());

/**
 * A run with nothing expanded. FRESH each time, never a shared singleton: the
 * sets are typed `ReadonlySet` but nothing at runtime stops a caller adding to
 * one, and a shared empty would then be every OTHER run's expansion too.
 */
function noExpansion(): WorkflowRunMapExpansion {
  return { waves: new Set(), compositions: new Set(), lanes: new Set() };
}

/**
 * Read one run's expansion. A run with no entry gets a FRESH empty pair, never
 * a shared one — see `noExpansion`.
 *
 * Deliberately a pure read: the map calls this from a `$derived`, so a write
 * here would invalidate the very derivation that made it. Use is recorded by
 * `touchRunMapExpansion` at the navigation that lands on the run instead, which
 * is also the more honest definition of "recently used".
 */
export function getWorkflowRunMapExpansion(runId: string): WorkflowRunMapExpansion {
  return runMapExpansion.get(runId) ?? noExpansion();
}

function toggled(set: ReadonlySet<string>, id: string): Set<string> {
  const next = new Set(set);
  if (!next.delete(id)) next.add(id);
  return next;
}

/**
 * Count a VISIT to whatever run the stack now rests on as use of its expansion.
 *
 * Eviction is least-recently-USED, and only toggles counted as use — so a run
 * someone expanded, came back to twice and never re-toggled aged out ahead of
 * runs they had opened once and left, and its waves silently re-folded on the
 * next visit. Every stack write calls this; a top that is not a run, or a run
 * with nothing expanded, has nothing to keep.
 */
function touchRunMapExpansion(): void {
  const top = getWorkflowsOverlayTop();
  if (top.level !== 'run') return;
  const entry = runMapExpansion.get(top.itemId);
  if (!entry) return;
  const keys = [...runMapExpansion.keys()];
  // Already the newest — rewriting would wake every reader to say nothing.
  if (keys[keys.length - 1] === top.itemId) return;
  const next = new Map(runMapExpansion);
  next.delete(top.itemId);
  next.set(top.itemId, entry);
  runMapExpansion = next;
}

/** Re-insert so the map orders least- to most-recently touched, then trim. */
function writeExpansion(runId: string, entry: WorkflowRunMapExpansion): void {
  const next = new Map(runMapExpansion);
  next.delete(runId);
  // A run that expanded something and then collapsed it again holds nothing —
  // and an entry holding nothing is indistinguishable from having no entry,
  // except that it occupies a slot and evicts a run that DOES have state.
  if (entry.waves.size > 0 || entry.compositions.size > 0 || entry.lanes.size > 0) next.set(runId, entry);
  while (next.size > RUN_MAP_EXPANSION_LIMIT) {
    const oldest = next.keys().next();
    if (oldest.done) break;
    next.delete(oldest.value);
  }
  runMapExpansion = next;
}

export function toggleWorkflowRunMapWave(runId: string, waveItemId: string): void {
  if (!runId || !waveItemId) return;
  const current = runMapExpansion.get(runId) ?? noExpansion();
  writeExpansion(runId, { ...current, waves: toggled(current.waves, waveItemId) });
}

export function toggleWorkflowRunMapComposition(runId: string, itemId: string): void {
  if (!runId || !itemId) return;
  const current = runMapExpansion.get(runId) ?? noExpansion();
  writeExpansion(runId, { ...current, compositions: toggled(current.compositions, itemId) });
}

export function toggleWorkflowRunMapLane(runId: string, branchKey: string): void {
  if (!runId || !branchKey) return;
  const current = runMapExpansion.get(runId) ?? noExpansion();
  writeExpansion(runId, { ...current, lanes: toggled(current.lanes, branchKey) });
}

/**
 * Esc precedence (§2.2): disarm a confirm → close a dialog → pop. At home with
 * nothing armed or open there is nothing to consume and the pane stays: a
 * pane is closed by its close control or the chord, never by Escape.
 */
export type WorkflowsEscapeOutcome = 'disarmed' | 'dialog-closed' | 'popped' | null;

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
  return null;
}

/**
 * Open the workflows pane at one run's detail, inside the sweep — the
 * OS-notification deep link (§7) and the needs-attention row both land here.
 */
export function openWorkflowRunInPane(itemId: string, sweepIndex = -1): void {
  openWorkflowsPane();
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
  touchRunMapExpansion();
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
  touchRunMapExpansion();
}

export function resetWorkflowsOverlayForTest(): void {
  const fresh = defaults();
  stack = fresh.stack;
  projectFilter = fresh.projectFilter;
  sweepActive = fresh.sweepActive;
  sweepIndex = fresh.sweepIndex;
  dialog = null;
  armedAction = null;
  runMapExpansion = new Map();
}
