import type { TerminalSessionSummary } from '../../types/terminal';

export interface TerminalTabState {
  /** Wails-issued ID for the underlying PTY session. */
  terminalID: string;
  /** Pre-output summary from OpenTerminal/RestartTerminal/ListTerminals. */
  summary: TerminalSessionSummary;
  /** Incoming output bytes queue while the xterm instance is mounting. */
  pendingOutput: Uint8Array[];
}

export interface ThreadTerminalState {
  tabs: TerminalTabState[];
  activeTerminalID: string | null;
  drawerHeight: number;
}

/**
 * The slice of a mounted xterm that app commands drive through the handle
 * (terminal.clear). Kept to the actions used so the store never depends on
 * `@xterm/xterm` and the fakes in tests stay small.
 */
export interface TerminalXtermActions {
  clear(): void;
}

const DEFAULT_DRAWER_HEIGHT = 280;
const MIN_DRAWER_HEIGHT = 160;
const MAX_DRAWER_HEIGHT = 1200;

/**
 * Cap pendingOutput at 1 MB of PTY bytes per terminal tab. A runaway process
 * (e.g. `yes`) while TerminalBody is unmounted would otherwise grow the queue
 * without bound. Chunks are raw `Uint8Array` bytes straight from the PTY, so
 * `.length` is the exact byte count and the cap is precise.
 */
const PENDING_OUTPUT_BYTE_CAP = 1_000_000;
const TERMINAL_STATE_CACHE_LIMIT = 32;

export const PENDING_OUTPUT_LIMITS = {
  bytes: PENDING_OUTPUT_BYTE_CAP,
};

/**
 * Append `chunk` to a pending-output queue, dropping oldest chunks when the
 * total byte count would exceed the cap. If the single incoming chunk is
 * larger than the cap we slice its tail and discard the queue. Exported as a
 * pure helper so the logic is unit-testable without a Svelte state wrapper.
 */
function trimPendingOutputEntries(
  existing: Uint8Array[],
  chunk: Uint8Array,
  existingSequences: number[] | null = null,
  sequence = 0,
): { output: Uint8Array[]; sequences: number[] | null } {
  if (chunk.length === 0) {
    return { output: existing, sequences: existingSequences };
  }
  if (chunk.length >= PENDING_OUTPUT_BYTE_CAP) {
    // A single jumbo chunk exceeds the cap — keep just the tail of it.
    return {
      output: [chunk.slice(chunk.length - PENDING_OUTPUT_BYTE_CAP)],
      sequences: existingSequences ? [sequence] : null,
    };
  }
  let totalBytes = chunk.length;
  for (const s of existing) totalBytes += s.length;
  if (totalBytes <= PENDING_OUTPUT_BYTE_CAP) {
    return {
      output: [...existing, chunk],
      sequences: existingSequences ? [...existingSequences, sequence] : null,
    };
  }
  // Evict oldest whole chunks first, then slice into the next chunk if we
  // still overflow. The resulting queue is always <= the cap.
  const next = existing.slice();
  const nextSequences = existingSequences?.slice() ?? null;
  let size = totalBytes;
  while (next.length > 0 && size > PENDING_OUTPUT_BYTE_CAP) {
    const first = next[0]!;
    size -= first.length;
    next.shift();
    nextSequences?.shift();
  }
  if (size > PENDING_OUTPUT_BYTE_CAP && next.length > 0) {
    // Shouldn't happen with the loop above but keeps the invariant obvious.
    next.length = 0;
    if (nextSequences) nextSequences.length = 0;
    size = 0;
  }
  next.push(chunk);
  nextSequences?.push(sequence);
  return { output: next, sequences: nextSequences };
}

export function trimPendingOutput(existing: Uint8Array[], chunk: Uint8Array): Uint8Array[] {
  return trimPendingOutputEntries(existing, chunk).output;
}

function trimPendingOutputWithSequences(
  existing: Uint8Array[],
  existingSequences: number[],
  chunk: Uint8Array,
  sequence: number,
): { output: Uint8Array[]; sequences: number[] } {
  const trimmed = trimPendingOutputEntries(existing, chunk, existingSequences, sequence);
  return { output: trimmed.output, sequences: trimmed.sequences ?? [] };
}

/**
 * Creates a reactive state container for a thread's terminal surface. The
 * mounted drawer is currently the only renderer, but the state is deliberately
 * independent of that renderer so a future dock/overlay can attach to the same
 * thread-owned terminal model.
 */
export function createThreadTerminalState(): ThreadTerminalStateHandle {
  let tabs: TerminalTabState[] = $state([]);
  let activeTerminalID: string | null = $state(null);
  let drawerHeight: number = $state(DEFAULT_DRAWER_HEIGHT);
  const pendingSequencesByTerminal = new Map<string, number[]>();
  const replayWatermarkByTerminal = new Map<string, number>();
  // The mounted xterm per tab, registered by TerminalBody while it lives.
  // Plain Map, not $state: nothing renders from it, commands just call into it.
  const xtermByTerminal = new Map<string, TerminalXtermActions>();

  return {
    get tabs() { return tabs; },
    get activeTerminalID() { return activeTerminalID; },
    get drawerHeight() { return drawerHeight; },

    addTab(summary: TerminalSessionSummary, options?: { activate?: boolean }): void {
      // A tab this client opened, restarted or moved becomes the active one;
      // a tab another client opened (`terminal:opened`) lands beside the
      // active tab without taking focus from whatever this person is typing
      // into, and takes it only when there was nothing active.
      const activate = options?.activate ?? true;
      const existing = tabs.find((t) => t.terminalID === summary.terminalID);
      if (existing) {
        tabs = tabs.map((t) =>
          t.terminalID === summary.terminalID ? { ...t, summary } : t,
        );
        if (activate) activeTerminalID = summary.terminalID;
        return;
      }
      tabs = [
        ...tabs,
        {
          terminalID: summary.terminalID,
          summary,
          pendingOutput: [],
        },
      ];
      pendingSequencesByTerminal.set(summary.terminalID, []);
      replayWatermarkByTerminal.set(summary.terminalID, 0);
      if (activate || activeTerminalID === null) activeTerminalID = summary.terminalID;
    },

    removeTab(terminalID: string): void {
      const nextTabs = tabs.filter((t) => t.terminalID !== terminalID);
      tabs = nextTabs;
      pendingSequencesByTerminal.delete(terminalID);
      replayWatermarkByTerminal.delete(terminalID);
      xtermByTerminal.delete(terminalID);
      if (activeTerminalID === terminalID) {
        activeTerminalID = nextTabs.length > 0 ? nextTabs[nextTabs.length - 1]!.terminalID : null;
      }
    },

    setActive(terminalID: string): void {
      const match = tabs.find((t) => t.terminalID === terminalID);
      if (match) {
        activeTerminalID = terminalID;
      }
    },

    appendOutput(terminalID: string, data: Uint8Array, sequence = 0): void {
      const watermark = replayWatermarkByTerminal.get(terminalID) ?? 0;
      if (sequence > 0 && sequence <= watermark) return;
      tabs = tabs.map((t) => {
        if (t.terminalID !== terminalID) return t;
        const next = trimPendingOutputWithSequences(
          t.pendingOutput,
          pendingSequencesByTerminal.get(terminalID) ?? [],
          data,
          sequence,
        );
        pendingSequencesByTerminal.set(terminalID, next.sequences);
        return { ...t, pendingOutput: next.output };
      });
    },

    drainOutput(terminalID: string): Uint8Array[] {
      const match = tabs.find((t) => t.terminalID === terminalID);
      if (!match || match.pendingOutput.length === 0) return [];
      const drained = match.pendingOutput;
      pendingSequencesByTerminal.set(terminalID, []);
      tabs = tabs.map((t) =>
        t.terminalID === terminalID ? { ...t, pendingOutput: [] } : t,
      );
      return drained;
    },

    markReplayed(terminalID: string, fromSequence: number, throughSequence: number): void {
      const previous = replayWatermarkByTerminal.get(terminalID) ?? 0;
      const nextWatermark = Math.max(previous, throughSequence);
      replayWatermarkByTerminal.set(terminalID, nextWatermark);
      if (fromSequence <= 0 || nextWatermark <= 0) return;

      const sequences = pendingSequencesByTerminal.get(terminalID) ?? [];
      const match = tabs.find((t) => t.terminalID === terminalID);
      if (!match || match.pendingOutput.length === 0 || sequences.length === 0) return;

      const nextOutput: Uint8Array[] = [];
      const nextSequences: number[] = [];
      for (let i = 0; i < match.pendingOutput.length; i += 1) {
        const sequence = sequences[i] ?? 0;
        if (sequence > fromSequence && sequence <= nextWatermark) continue;
        nextOutput.push(match.pendingOutput[i]!);
        nextSequences.push(sequence);
      }
      pendingSequencesByTerminal.set(terminalID, nextSequences);
      tabs = tabs.map((t) =>
        t.terminalID === terminalID ? { ...t, pendingOutput: nextOutput } : t,
      );
    },

    updateSummary(summary: TerminalSessionSummary): void {
      tabs = tabs.map((t) =>
        t.terminalID === summary.terminalID ? { ...t, summary } : t,
      );
    },

    setDrawerHeight(height: number): void {
      drawerHeight = Math.max(MIN_DRAWER_HEIGHT, Math.min(MAX_DRAWER_HEIGHT, Math.round(height)));
    },

    attachXterm(terminalID: string, xterm: TerminalXtermActions): () => void {
      xtermByTerminal.set(terminalID, xterm);
      return () => {
        // Only drop our own registration: a remount for the same tab may have
        // attached its new xterm before the old surface's teardown ran.
        if (xtermByTerminal.get(terminalID) === xterm) xtermByTerminal.delete(terminalID);
      };
    },

    clearActive(): boolean {
      const xterm = activeTerminalID ? xtermByTerminal.get(activeTerminalID) : undefined;
      if (!xterm) return false;
      xterm.clear();
      return true;
    },

    clear(): void {
      tabs = [];
      activeTerminalID = null;
      pendingSequencesByTerminal.clear();
      replayWatermarkByTerminal.clear();
      xtermByTerminal.clear();
    },
  };
}

export interface ThreadTerminalStateHandle {
  readonly tabs: TerminalTabState[];
  readonly activeTerminalID: string | null;
  readonly drawerHeight: number;
  /** Add or refresh a tab. `activate: false` leaves the active tab alone unless there was none. */
  addTab(summary: TerminalSessionSummary, options?: { activate?: boolean }): void;
  removeTab(terminalID: string): void;
  setActive(terminalID: string): void;
  appendOutput(terminalID: string, data: Uint8Array, sequence?: number): void;
  drainOutput(terminalID: string): Uint8Array[];
  markReplayed(terminalID: string, fromSequence: number, throughSequence: number): void;
  updateSummary(summary: TerminalSessionSummary): void;
  setDrawerHeight(height: number): void;
  /**
   * Register a tab's mounted xterm so app commands can reach it. Returns the
   * detach; a later attach for the same tab (remount) supersedes the earlier one.
   */
  attachXterm(terminalID: string, xterm: TerminalXtermActions): () => void;
  /**
   * Wipe the active tab's xterm buffer and scrollback (the terminal.clear
   * command). Frontend-only: nothing goes to the PTY. Returns false when no
   * active tab has a mounted xterm.
   */
  clearActive(): boolean;
  clear(): void;
}

export const TERMINAL_DRAWER_LIMITS = {
  min: MIN_DRAWER_HEIGHT,
  max: MAX_DRAWER_HEIGHT,
  default: DEFAULT_DRAWER_HEIGHT,
};

const terminalStatesByThread = new Map<string, ThreadTerminalStateHandle>();

function terminalStateKey(threadID: string): string {
  return threadID || '__unbound__';
}

/**
 * Resolves the identity a pane's terminal state is keyed under: its thread when
 * bound, otherwise its pane id so an unbound/draft pane keeps its own isolated
 * terminals. Pass the result to getThreadTerminalState /
 * getExistingThreadTerminalState, which apply the unbound-thread normalization.
 *
 * This is the single source of truth for the key so the terminal surfaces and
 * the keybinding command can't drift: a refresh or command must resolve the
 * SAME state the surface mounted, or it targets the wrong terminal.
 */
export function terminalStateKeyForPane(
  threadId: string | null | undefined,
  paneId: string,
): string {
  return threadId ?? paneId;
}

export function getThreadTerminalState(threadID: string): ThreadTerminalStateHandle {
  const key = terminalStateKey(threadID);
  const existing = terminalStatesByThread.get(key);
  if (existing) return existing;
  const handle = createThreadTerminalState();
  terminalStatesByThread.set(key, handle);
  evictEmptyTerminalStates();
  return handle;
}

export function getExistingThreadTerminalState(
  threadID: string,
): ThreadTerminalStateHandle | null {
  return terminalStatesByThread.get(terminalStateKey(threadID)) ?? null;
}

export function getThreadTerminalStateForTerminalEvent(
  threadID: string,
  terminalID: string,
): ThreadTerminalStateHandle {
  const key = terminalStateKey(threadID);
  const eventHandle = terminalStatesByThread.get(key);
  if (eventHandle?.tabs.some((tab) => tab.terminalID === terminalID)) {
    return eventHandle;
  }
  for (const handle of terminalStatesByThread.values()) {
    if (handle.tabs.some((tab) => tab.terminalID === terminalID)) {
      return handle;
    }
  }
  return getThreadTerminalState(threadID);
}

export function clearThreadTerminalState(threadID: string): void {
  const key = terminalStateKey(threadID);
  const handle = terminalStatesByThread.get(key);
  handle?.clear();
  terminalStatesByThread.delete(key);
}

export function migrateThreadTerminalState(
  fromThreadID: string,
  toThreadID: string,
  summaries: TerminalSessionSummary[] = [],
): void {
  const fromKey = terminalStateKey(fromThreadID);
  const toKey = terminalStateKey(toThreadID);
  if (fromKey === toKey) return;

  const source = terminalStatesByThread.get(fromKey);
  if (!source) return;

  const summaryByID = new Map(summaries.map((summary) => [summary.terminalID, summary]));
  for (const tab of source.tabs) {
    source.updateSummary(
      summaryByID.get(tab.terminalID) ?? {
        ...tab.summary,
        threadID: toThreadID,
      },
    );
  }

  const target = terminalStatesByThread.get(toKey);
  if (target && target !== source && target.tabs.length > 0) {
    for (const tab of source.tabs) {
      target.addTab(
        summaryByID.get(tab.terminalID) ?? {
          ...tab.summary,
          threadID: toThreadID,
        },
      );
    }
    source.clear();
  } else {
    terminalStatesByThread.set(toKey, source);
  }
  terminalStatesByThread.delete(fromKey);
}

export function releaseThreadTerminalState(threadID: string): void {
  terminalStatesByThread.delete(terminalStateKey(threadID));
}

export function resetThreadTerminalStatesForTest(): void {
  terminalStatesByThread.clear();
}

function evictEmptyTerminalStates(): void {
  if (terminalStatesByThread.size <= TERMINAL_STATE_CACHE_LIMIT) return;
  for (const [threadID, handle] of terminalStatesByThread) {
    if (terminalStatesByThread.size <= TERMINAL_STATE_CACHE_LIMIT) return;
    if (handle.tabs.length > 0) continue;
    terminalStatesByThread.delete(threadID);
  }
}

// Module-level terminal-focus registry, keyed by paneId. The keybindings
// dispatcher reads `getTerminalFocused(paneId)` via makeCommandContext to gate
// a specific pane's `terminalFocus`-scoped chords. TerminalBody
// registers/deregisters via `notifyTerminalFocus(paneId, focused)` — the
// per-pane count tolerates multiple mounts (e.g. keyed remount when swapping
// the active tab) without flipping that pane's flag prematurely. Keying by pane
// is what keeps two terminal panes independent: focusing one must not suppress
// the other pane's `!terminalFocus` chords. Entries are deleted at count zero so
// the map only holds panes that currently have a focused terminal.
const focusedTerminalsByPane = new Map<string, number>();

export function getTerminalFocused(paneId: string): boolean {
  return (focusedTerminalsByPane.get(paneId) ?? 0) > 0;
}

export function notifyTerminalFocus(paneId: string, focused: boolean): void {
  const current = focusedTerminalsByPane.get(paneId) ?? 0;
  if (focused) {
    focusedTerminalsByPane.set(paneId, current + 1);
    return;
  }
  const next = Math.max(0, current - 1);
  if (next === 0) {
    focusedTerminalsByPane.delete(paneId);
  } else {
    focusedTerminalsByPane.set(paneId, next);
  }
}

/** Test helper — clear the registry between suites. */
export function resetTerminalFocusForTest(): void {
  focusedTerminalsByPane.clear();
}
