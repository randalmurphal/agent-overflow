import type { TerminalSessionSummary } from '../../types/terminal';
import { dropPaneFindRegion } from '../../stores/paneFind.svelte';
import {
  clearWarmTerminals,
  evictWarmTerminal,
  evictWarmTerminalsForHandle,
} from './terminalWarmPool';

/** The find region of a terminal's tab, under every pane that shows it
 * (`terminalFindKey`). */
export function terminalFindRegion(terminalID: string): string {
  return `terminal:${terminalID}`;
}

export interface TerminalTabState {
  /** Wails-issued ID for the underlying PTY session. */
  terminalID: string;
  /** Pre-output summary from OpenTerminal/ListTerminals. */
  summary: TerminalSessionSummary;
  /** Incoming output bytes queue while the xterm instance is mounting. */
  pendingOutput: Uint8Array[];
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
 * Appends `chunk`, numbered `sequence`, to a pending-output queue and its
 * parallel sequence list, evicting the oldest whole chunks while the total
 * would exceed the cap. A chunk that alone reaches the cap replaces the
 * queue with its own tail.
 */
function appendPendingOutput(
  existing: Uint8Array[],
  existingSequences: number[],
  existingBytes: number,
  chunk: Uint8Array,
  sequence: number,
): { output: Uint8Array[]; sequences: number[]; bytes: number; overflowed: boolean } {
  if (chunk.length === 0) {
    return { output: existing, sequences: existingSequences, bytes: existingBytes, overflowed: false };
  }
  if (chunk.length >= PENDING_OUTPUT_BYTE_CAP) {
    return {
      output: [chunk.slice(chunk.length - PENDING_OUTPUT_BYTE_CAP)],
      sequences: [sequence],
      bytes: PENDING_OUTPUT_BYTE_CAP,
      overflowed: existing.length > 0 || chunk.length > PENDING_OUTPUT_BYTE_CAP,
    };
  }
  const output = [...existing, chunk];
  const sequences = [...existingSequences, sequence];
  let size = existingBytes + chunk.length;
  let overflowed = false;
  let dropCount = 0;
  while (size > PENDING_OUTPUT_BYTE_CAP && dropCount < output.length) {
    size -= output[dropCount]!.length;
    dropCount += 1;
    overflowed = true;
  }
  if (dropCount > 0) {
    return {
      output: output.slice(dropCount),
      sequences: sequences.slice(dropCount),
      bytes: size,
      overflowed,
    };
  }
  return { output, sequences, bytes: size, overflowed };
}

/**
 * Creates a reactive state container for a thread's terminal surface. It is
 * independent of the surface that renders it: the thread's drawer and the
 * terminal pane (TerminalView) both mount TerminalSurface over it.
 */
export function createThreadTerminalState(): ThreadTerminalStateHandle {
  let tabs: TerminalTabState[] = $state([]);
  let activeTerminalID: string | null = $state(null);
  let drawerHeight: number = $state(DEFAULT_DRAWER_HEIGHT);
  let released = false;
  const pendingSequencesByTerminal = new Map<string, number[]>();
  const pendingBytesByTerminal = new Map<string, number>();
  const replayWatermarkByTerminal = new Map<string, number>();
  // The mounted xterm per tab, registered by TerminalBody while it lives.
  // Plain Map, not $state: nothing renders from it, commands just call into it.
  const xtermByTerminal = new Map<string, TerminalXtermActions>();
  // Direct PTY output sink registered by a hydrated TerminalBody so live
  // chunks bypass reactive `tabs` cloning and $effect wakeups.
  const outputSinkByTerminal = new Map<string, (chunk: Uint8Array) => void>();
  // Per list read in flight, the tabs added or removed since it left.
  const listReadsInFlight = new Set<Set<string>>();

  function noteTabChange(terminalID: string): void {
    for (const changed of listReadsInFlight) changed.add(terminalID);
  }

  function addTab(summary: TerminalSessionSummary, options?: { activate?: boolean }): void {
    noteTabChange(summary.terminalID);
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
    pendingBytesByTerminal.set(summary.terminalID, 0);
    replayWatermarkByTerminal.set(summary.terminalID, 0);
    if (activate || activeTerminalID === null) activeTerminalID = summary.terminalID;
  }

  // A tab goes only when its terminal ended, so its find state goes with
  // it, whether or not a surface shows the tab.
  function removeTab(terminalID: string): void {
    noteTabChange(terminalID);
    dropPaneFindRegion(terminalFindRegion(terminalID));
    evictWarmTerminal(terminalID);
    const nextTabs = tabs.filter((t) => t.terminalID !== terminalID);
    tabs = nextTabs;
    pendingSequencesByTerminal.delete(terminalID);
    pendingBytesByTerminal.delete(terminalID);
    replayWatermarkByTerminal.delete(terminalID);
    xtermByTerminal.delete(terminalID);
    outputSinkByTerminal.delete(terminalID);
    if (activeTerminalID === terminalID) {
      activeTerminalID = nextTabs.length > 0 ? nextTabs[nextTabs.length - 1]!.terminalID : null;
    }
  }

  const handle: ThreadTerminalStateHandle = {
    get tabs() { return tabs; },
    get activeTerminalID() { return activeTerminalID; },
    get drawerHeight() { return drawerHeight; },

    hasTab(terminalID: string): boolean {
      return tabs.some((t) => t.terminalID === terminalID);
    },

    addTab,
    removeTab,

    async syncTabs(read, options) {
      const changed = new Set<string>();
      listReadsInFlight.add(changed);
      let list: TerminalSessionSummary[] | null;
      try {
        list = await read();
      } finally {
        listReadsInFlight.delete(changed);
      }
      if (!list || !options.current()) return;
      const listed = new Set(list.map((summary) => summary.terminalID));
      const gone = tabs
        .map((tab) => tab.terminalID)
        .filter((terminalID) => !listed.has(terminalID) && !changed.has(terminalID));
      for (const terminalID of gone) options.remove(terminalID);
      for (const summary of list) {
        if (!changed.has(summary.terminalID)) addTab(summary, { activate: options.activate });
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
      if (data.length === 0) return;
      const sink = outputSinkByTerminal.get(terminalID);
      if (sink) {
        if (sequence > watermark) replayWatermarkByTerminal.set(terminalID, sequence);
        sink(data);
        return;
      }
      const idx = tabs.findIndex((t) => t.terminalID === terminalID);
      if (idx < 0) return;
      const tab = tabs[idx]!;
      const next = appendPendingOutput(
        tab.pendingOutput,
        pendingSequencesByTerminal.get(terminalID) ?? [],
        pendingBytesByTerminal.get(terminalID) ?? 0,
        data,
        sequence,
      );
      pendingSequencesByTerminal.set(terminalID, next.sequences);
      pendingBytesByTerminal.set(terminalID, next.bytes);
      if (next.overflowed) evictWarmTerminal(terminalID);
      const nextTabs = tabs.slice();
      nextTabs[idx] = { ...tab, pendingOutput: next.output };
      tabs = nextTabs;
    },

    drainOutput(terminalID: string): Uint8Array[] {
      const idx = tabs.findIndex((t) => t.terminalID === terminalID);
      if (idx < 0) return [];
      const match = tabs[idx]!;
      if (match.pendingOutput.length === 0) return [];
      const drained = match.pendingOutput;
      const sequences = pendingSequencesByTerminal.get(terminalID);
      if (sequences && sequences.length > 0) {
        let maxSeq = replayWatermarkByTerminal.get(terminalID) ?? 0;
        for (let i = 0; i < sequences.length; i += 1) {
          const s = sequences[i]!;
          if (s > maxSeq) maxSeq = s;
        }
        if (maxSeq > 0) replayWatermarkByTerminal.set(terminalID, maxSeq);
      }
      pendingSequencesByTerminal.set(terminalID, []);
      pendingBytesByTerminal.set(terminalID, 0);
      const nextTabs = tabs.slice();
      nextTabs[idx] = { ...match, pendingOutput: [] };
      tabs = nextTabs;
      return drained;
    },

    markReplayed(terminalID: string, fromSequence: number, throughSequence: number): void {
      const previous = replayWatermarkByTerminal.get(terminalID) ?? 0;
      const nextWatermark = Math.max(previous, throughSequence);
      replayWatermarkByTerminal.set(terminalID, nextWatermark);
      if (fromSequence <= 0 || nextWatermark <= 0) return;

      const sequences = pendingSequencesByTerminal.get(terminalID) ?? [];
      const idx = tabs.findIndex((t) => t.terminalID === terminalID);
      const match = idx >= 0 ? tabs[idx] : undefined;
      if (!match || match.pendingOutput.length === 0 || sequences.length === 0) return;

      const nextOutput: Uint8Array[] = [];
      const nextSequences: number[] = [];
      let nextBytes = 0;
      for (let i = 0; i < match.pendingOutput.length; i += 1) {
        const sequence = sequences[i] ?? 0;
        if (sequence > fromSequence && sequence <= nextWatermark) continue;
        const chunk = match.pendingOutput[i]!;
        nextOutput.push(chunk);
        nextSequences.push(sequence);
        nextBytes += chunk.length;
      }
      pendingSequencesByTerminal.set(terminalID, nextSequences);
      pendingBytesByTerminal.set(terminalID, nextBytes);
      const nextTabs = tabs.slice();
      nextTabs[idx] = { ...match, pendingOutput: nextOutput };
      tabs = nextTabs;
    },

    updateSummary(summary: TerminalSessionSummary): void {
      const idx = tabs.findIndex((t) => t.terminalID === summary.terminalID);
      if (idx < 0) return;
      const nextTabs = tabs.slice();
      nextTabs[idx] = { ...tabs[idx]!, summary };
      tabs = nextTabs;
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

    attachOutputSink(terminalID: string, sink: (chunk: Uint8Array) => void): () => void {
      outputSinkByTerminal.set(terminalID, sink);
      return () => {
        if (outputSinkByTerminal.get(terminalID) === sink) outputSinkByTerminal.delete(terminalID);
      };
    },

    clearActive(): boolean {
      const xterm = activeTerminalID ? xtermByTerminal.get(activeTerminalID) : undefined;
      if (!xterm) return false;
      xterm.clear();
      return true;
    },

    clear(): void {
      evictWarmTerminalsForHandle(handle);
      tabs = [];
      activeTerminalID = null;
      pendingSequencesByTerminal.clear();
      pendingBytesByTerminal.clear();
      replayWatermarkByTerminal.clear();
      xtermByTerminal.clear();
      outputSinkByTerminal.clear();
    },

    isReleased(): boolean {
      return released;
    },
  };

  releasedStateMark.set(handle, () => {
    released = true;
  });

  return handle;
}

const releasedStateMark = new WeakMap<ThreadTerminalStateHandle, () => void>();

export interface ThreadTerminalStateHandle {
  readonly tabs: TerminalTabState[];
  readonly activeTerminalID: string | null;
  readonly drawerHeight: number;
  hasTab(terminalID: string): boolean;
  /** Add or refresh a tab. `activate: false` leaves the active tab alone unless there was none. */
  addTab(summary: TerminalSessionSummary, options?: { activate?: boolean }): void;
  removeTab(terminalID: string): void;
  /**
   * Make the tabs match `read`'s list of the thread's terminals: hand each
   * tab it lacks to `remove`, which must remove it, and add or refresh the
   * ones it names. A tab added or removed while the read was in flight keeps
   * that change, which is newer than the list. Nothing changes when the
   * read answers null or `current` says its answer no longer applies. A
   * failed read rejects and changes nothing.
   */
  syncTabs(
    read: () => Promise<TerminalSessionSummary[] | null>,
    options: { activate: boolean; current(): boolean; remove(terminalID: string): void },
  ): Promise<void>;
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
   * Register a direct live PTY output sink for a hydrated terminal body,
   * avoiding reactive `tabs` cloning per output chunk.
   */
  attachOutputSink(terminalID: string, sink: (chunk: Uint8Array) => void): () => void;
  /**
   * Wipe the active tab's xterm buffer and scrollback (the terminal.clear
   * command). Frontend-only: nothing goes to the PTY. Returns false when no
   * active tab has a mounted xterm.
   */
  clearActive(): boolean;
  clear(): void;
  /** True once `releaseThreadTerminalState` has retired this handle (thread
   * deleted or backend detached), so an unmounting TerminalBody disposes its
   * WebGL2 xterm instead of parking an unreachable zombie in the warm pool. */
  isReleased(): boolean;
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

/** Forgets a thread's terminals, which ended with it, and their find state. */
export function clearThreadTerminalState(threadID: string): void {
  const key = terminalStateKey(threadID);
  const handle = terminalStatesByThread.get(key);
  for (const tab of handle?.tabs ?? []) dropPaneFindRegion(terminalFindRegion(tab.terminalID));
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
  const key = terminalStateKey(threadID);
  const existing = terminalStatesByThread.get(key);
  if (existing) {
    releasedStateMark.get(existing)?.();
    evictWarmTerminalsForHandle(existing);
  }
  terminalStatesByThread.delete(key);
}

/** A mounted surface: its thread and its re-read of that thread's terminal list. */
export interface MountedTerminalSurface {
  readonly threadID: string;
  relist(): Promise<void>;
}

const mountedTerminalSurfaces = new Set<MountedTerminalSurface>();

/**
 * Register a mounted surface so a computer that reconnects or loses terminal
 * events can re-read its list (../../stores/eventsTerminal.ts). Returns the
 * unregister.
 */
export function registerTerminalSurface(threadID: string, relist: () => Promise<void>): () => void {
  const surface: MountedTerminalSurface = { threadID, relist };
  mountedTerminalSurfaces.add(surface);
  return () => {
    mountedTerminalSurfaces.delete(surface);
  };
}

export function getMountedTerminalSurfaces(): MountedTerminalSurface[] {
  return [...mountedTerminalSurfaces];
}

export function resetThreadTerminalStatesForTest(): void {
  clearWarmTerminals();
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

// Module-level terminal-focus registry, keyed by paneId. A pane's terminal has
// focus while keyboard focus is inside its surface: the xterm, or the find
// bar over it. Every element of the surface that can hold focus is a region
// (`terminalFocusRegion`) that reports through `notifyTerminalFocus(paneId,
// focused)`; the per-pane count tolerates several regions and keyed remounts
// (swapping the active tab) without flipping the flag prematurely. Keying by
// pane keeps two terminal panes independent. Entries are deleted at count
// zero so the map only holds panes that currently have a focused terminal.
// The keybinding context's `terminalFocus` is narrower: keys reach the shell
// only from the xterm (makeCommandContext).
const focusedTerminalsByPane = new Map<string, number>();

/** Whether keyboard focus is inside the pane's terminal surface. */
export function getTerminalFocused(paneId: string): boolean {
  return (focusedTerminalsByPane.get(paneId) ?? 0) > 0;
}

/** Counts focus inside `node` as the pane's terminal focus while it is
 * attached. */
/** The selector of a terminal's focus region (terminalFocusRegion): where a press is on the terminal. */
export const TERMINAL_FOCUS_REGION = '[data-terminal-focus-region]';

export function terminalFocusRegion(paneId: string): (node: HTMLElement) => () => void {
  return (node) => {
    node.dataset.terminalFocusRegion = '';
    let counted = false;
    const set = (focused: boolean) => {
      if (counted === focused) return;
      counted = focused;
      notifyTerminalFocus(paneId, focused);
    };
    // Focus moving inside the region leaves and re-enters it in one turn.
    const onIn = () => set(true);
    const onOut = () => set(false);
    node.addEventListener('focusin', onIn);
    node.addEventListener('focusout', onOut);
    if (node.contains(document.activeElement)) set(true);
    return () => {
      node.removeEventListener('focusin', onIn);
      node.removeEventListener('focusout', onOut);
      set(false);
    };
  };
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
