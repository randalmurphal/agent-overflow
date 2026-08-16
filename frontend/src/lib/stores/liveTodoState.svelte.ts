import type { TodoStep } from '../types/events';
import type { ThreadLiveState } from '../../../bindings/agent-overflow/models';

/**
 * LiveTodo is the snapshot the activity rail's Todos segment renders.
 * Populated from `provider:todo_update` events (Claude TodoWrite reroute
 * + Codex update_plan, both normalised in the parser). Survives turn
 * boundaries by design: the segment keeps showing while items remain
 * incomplete and auto-hides on a timer when every step is `completed`.
 */
export interface LiveTodo {
  steps: TodoStep[];
}

/**
 * LIVE_TODO_AUTOHIDE_MS is how long the snapshot lingers after every
 * step is `completed` before the auto-hide timer clears it. Long
 * enough for the user to see the satisfying all-done state, short
 * enough that the segment doesn't squat on the rail indefinitely.
 *
 * It MUST agree with the backend's `liveTodoAutoHideMillis`
 * (`app_live_state.go`): the list is durable now
 * (`threads.live_todo`), so `GetThreadLiveState` applies the same age
 * filter on hydration — if the two disagreed, a refresh after the
 * timer would resurrect a list this pane had already hidden.
 */
export const LIVE_TODO_AUTOHIDE_MS = 5_000;

/**
 * Per-thread live-todo dropdown UI preferences (show-all reveal).
 * Module-scoped so a thread switch can save the outgoing thread's
 * state and restore the incoming thread's. Lives in process memory by
 * design — survives thread switches within a session, dies on app
 * restart, no SQLite roundtrip.
 */
interface LiveTodoUiPrefs {
  showAll: boolean;
}
const liveTodoUiPrefs = new Map<string, LiveTodoUiPrefs>();

function readLiveTodoUiPrefs(threadID: string | null): LiveTodoUiPrefs {
  if (!threadID) return { showAll: false };
  return liveTodoUiPrefs.get(threadID) ?? { showAll: false };
}

function writeLiveTodoUiPrefs(threadID: string | null, prefs: LiveTodoUiPrefs): void {
  if (!threadID) return;
  liveTodoUiPrefs.set(threadID, prefs);
}

/**
 * Drop a thread's live-todo UI prefs. Called from the thread-removal
 * path so a deleted thread doesn't leave a permanent entry in the
 * module-scoped prefs map. Bounded growth would otherwise be tied to
 * the count of distinct threads ever toggled in a session, which is
 * fine in practice but accumulates across long-running sessions.
 */
export function dropLiveTodoUiPrefs(threadID: string | null): void {
  if (!threadID) return;
  liveTodoUiPrefs.delete(threadID);
}

/**
 * Test-only reset for the live-todo UI prefs map. The map is
 * intentionally module-scoped so per-thread open/closed state survives
 * thread switches in production; tests need to clear it between cases
 * so cross-test pollution doesn't flip a fresh pane's defaults.
 * Production code never calls this — same pattern as the markdown
 * enhancement caches in `markdownEnhance.ts`.
 */
export function __resetLiveTodoUiPrefsForTest(): void {
  liveTodoUiPrefs.clear();
}

/**
 * Per-thread Activity Rail expansion state. The rail itself appears only
 * when there's active work (turn / todos / background tasks / a pending
 * user-input request); these flags govern the section bodies below the rail.
 * `todosOpen`/`backgroundOpen` are opt-in-open (default closed);
 * `inputCollapsed` is the inverse — the pending-input popup defaults to
 * expanded and this records that the user minimized it. Independent toggles.
 * Same shape and lifecycle rules as `liveTodoUiPrefs`: lives in process
 * memory, survives thread switches, dies on app restart.
 */
interface ActivityRailUiPrefs {
  todosOpen: boolean;
  backgroundOpen: boolean;
  inputCollapsed: boolean;
}

function defaultActivityRailUiPrefs(): ActivityRailUiPrefs {
  return { todosOpen: false, backgroundOpen: false, inputCollapsed: false };
}

const activityRailUiPrefs = new Map<string, ActivityRailUiPrefs>();

function readActivityRailUiPrefs(threadID: string | null): ActivityRailUiPrefs {
  if (!threadID) return defaultActivityRailUiPrefs();
  return activityRailUiPrefs.get(threadID) ?? defaultActivityRailUiPrefs();
}

function writeActivityRailUiPrefs(threadID: string | null, prefs: ActivityRailUiPrefs): void {
  if (!threadID) return;
  activityRailUiPrefs.set(threadID, prefs);
}

export function dropActivityRailUiPrefs(threadID: string | null): void {
  if (!threadID) return;
  activityRailUiPrefs.delete(threadID);
}

export function __resetActivityRailUiPrefsForTest(): void {
  activityRailUiPrefs.clear();
}

type LiveTodoSnapshot = NonNullable<ThreadLiveState['todo']>;

export interface LiveTodoState {
  readonly liveTodo: LiveTodo | null;
  readonly liveTodoShowAll: boolean;
  readonly activityRailTodosOpen: boolean;
  readonly activityRailBackgroundOpen: boolean;
  readonly activityRailInputCollapsed: boolean;
  readonly revision: number;

  setLiveTodo(steps: TodoStep[]): void;
  clearLiveTodo(): void;
  hydrateSnapshotIfUnchanged(
    snapshot: ThreadLiveState['todo'] | null | undefined,
    threadID: string,
    revisionAtRequest: number,
  ): void;
  resetForThread(threadID: string): void;
  resetForEmptyPane(): void;
  toggleLiveTodoShowAll(threadID: string | null): void;
  toggleActivityRailTodos(threadID: string | null): void;
  toggleActivityRailBackground(threadID: string | null): void;
  toggleActivityRailInputCollapsed(threadID: string | null): void;
}

function shouldHydrateLiveTodoSnapshot(snapshot: LiveTodoSnapshot): boolean {
  if (!Array.isArray(snapshot.steps) || snapshot.steps.length === 0) {
    return false;
  }
  const allCompleted = snapshot.steps.every((step) => step.status === 'completed');
  if (!allCompleted) return true;
  const age = Date.now() - snapshot.updatedAt;
  return age >= 0 && age <= LIVE_TODO_AUTOHIDE_MS;
}

export function createLiveTodoState(): LiveTodoState {
  let liveTodo: LiveTodo | null = $state(null);
  let liveTodoShowAll = $state(false);
  let activityRailTodosOpen = $state(false);
  let activityRailBackgroundOpen = $state(false);
  let activityRailInputCollapsed = $state(false);

  function persistActivityRailUiPrefs(threadID: string | null): void {
    writeActivityRailUiPrefs(threadID, {
      todosOpen: activityRailTodosOpen,
      backgroundOpen: activityRailBackgroundOpen,
      inputCollapsed: activityRailInputCollapsed,
    });
  }

  let autoHideTimer: ReturnType<typeof setTimeout> | null = null;
  let clearedSteps = new Set<string>();
  let revision = 0;
  const clearedStepsCap = 1_000;

  function cancelAutoHide(): void {
    if (autoHideTimer !== null) {
      clearTimeout(autoHideTimer);
      autoHideTimer = null;
    }
  }

  function clearLiveTodoState(): void {
    revision += 1;
    cancelAutoHide();
    liveTodo = null;
    clearedSteps = new Set();
  }

  function setLiveTodoState(steps: TodoStep[]): void {
    revision += 1;
    cancelAutoHide();

    const filtered = clearedSteps.size === 0
      ? steps
      : steps.filter(
          (step) => !(step.status === 'completed' && clearedSteps.has(step.step)),
        );
    if (filtered.length === 0) {
      liveTodo = null;
      return;
    }

    liveTodo = { steps: filtered };
    const allComplete = filtered.every((step) => step.status === 'completed');
    if (allComplete) {
      autoHideTimer = setTimeout(() => {
        if (liveTodo) {
          for (const step of liveTodo.steps) {
            clearedSteps.add(step.step);
          }
          if (clearedSteps.size > clearedStepsCap) {
            const retained = Array.from(clearedSteps).slice(-clearedStepsCap);
            clearedSteps = new Set(retained);
          }
        }
        liveTodo = null;
        autoHideTimer = null;
      }, LIVE_TODO_AUTOHIDE_MS);
    }
  }

  function resetForThread(threadID: string): void {
    cancelAutoHide();
    liveTodo = null;
    clearedSteps = new Set();

    const todoPrefs = readLiveTodoUiPrefs(threadID);
    liveTodoShowAll = todoPrefs.showAll;

    const railPrefs = readActivityRailUiPrefs(threadID);
    activityRailTodosOpen = railPrefs.todosOpen;
    activityRailBackgroundOpen = railPrefs.backgroundOpen;
    activityRailInputCollapsed = railPrefs.inputCollapsed;
  }

  function resetForEmptyPane(): void {
    cancelAutoHide();
    liveTodo = null;
    clearedSteps = new Set();
    liveTodoShowAll = false;
    activityRailTodosOpen = false;
    activityRailBackgroundOpen = false;
    activityRailInputCollapsed = false;
  }

  return {
    get liveTodo() { return liveTodo; },
    get liveTodoShowAll() { return liveTodoShowAll; },
    get activityRailTodosOpen() { return activityRailTodosOpen; },
    get activityRailBackgroundOpen() { return activityRailBackgroundOpen; },
    get activityRailInputCollapsed() { return activityRailInputCollapsed; },
    get revision() { return revision; },

    setLiveTodo(steps: TodoStep[]): void {
      setLiveTodoState(steps);
    },

    clearLiveTodo(): void {
      clearLiveTodoState();
    },

    hydrateSnapshotIfUnchanged(
      snapshot: ThreadLiveState['todo'] | null | undefined,
      threadID: string,
      revisionAtRequest: number,
    ): void {
      if (revision !== revisionAtRequest) return;
      if (snapshot && snapshot.threadId === threadID && shouldHydrateLiveTodoSnapshot(snapshot)) {
        setLiveTodoState(snapshot.steps as TodoStep[]);
      } else {
        clearLiveTodoState();
      }
    },

    resetForThread,
    resetForEmptyPane,

    toggleLiveTodoShowAll(threadID: string | null): void {
      liveTodoShowAll = !liveTodoShowAll;
      writeLiveTodoUiPrefs(threadID, {
        showAll: liveTodoShowAll,
      });
    },

    toggleActivityRailTodos(threadID: string | null): void {
      activityRailTodosOpen = !activityRailTodosOpen;
      persistActivityRailUiPrefs(threadID);
    },

    toggleActivityRailBackground(threadID: string | null): void {
      activityRailBackgroundOpen = !activityRailBackgroundOpen;
      persistActivityRailUiPrefs(threadID);
    },

    toggleActivityRailInputCollapsed(threadID: string | null): void {
      activityRailInputCollapsed = !activityRailInputCollapsed;
      persistActivityRailUiPrefs(threadID);
    },
  };
}
