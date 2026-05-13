import type { TerminalSessionSummary } from '../../types/terminal';

export interface TerminalTabState {
  /** Wails-issued ID for the underlying PTY session. */
  terminalID: string;
  /** Pre-output summary from OpenTerminal/RestartTerminal/ListTerminals. */
  summary: TerminalSessionSummary;
  /** Incoming output events queue while the xterm instance is mounting. */
  pendingOutput: string[];
}

export interface ThreadTerminalState {
  tabs: TerminalTabState[];
  activeTerminalID: string | null;
  drawerHeight: number;
}

const DEFAULT_DRAWER_HEIGHT = 280;
const MIN_DRAWER_HEIGHT = 160;
const MAX_DRAWER_HEIGHT = 1200;

/**
 * Cap pendingOutput at roughly 1 MB of UTF-16 characters per terminal tab.
 * A runaway process (e.g. `yes`) while TerminalBody is unmounted would
 * otherwise grow the queue without bound. We measure by `.length` because
 * that's what the xterm sink writes anyway; a few-byte overshoot due to
 * multi-byte characters is acceptable.
 */
const PENDING_OUTPUT_CHAR_CAP = 1_000_000;
const TERMINAL_STATE_CACHE_LIMIT = 32;

export const PENDING_OUTPUT_LIMITS = {
  chars: PENDING_OUTPUT_CHAR_CAP,
};

/**
 * Append `chunk` to a pending-output queue, dropping oldest chunks when the
 * total character count would exceed the cap. If the single incoming chunk
 * is larger than the cap we slice its tail and discard the queue. Exported
 * as a pure helper so the logic is unit-testable without a Svelte state
 * wrapper.
 */
function trimPendingOutputEntries(
  existing: string[],
  chunk: string,
  existingSequences: number[] | null = null,
  sequence = 0,
): { output: string[]; sequences: number[] | null } {
  if (chunk.length === 0) {
    return { output: existing, sequences: existingSequences };
  }
  if (chunk.length >= PENDING_OUTPUT_CHAR_CAP) {
    // A single jumbo chunk exceeds the cap — keep just the tail of it.
    return {
      output: [chunk.slice(chunk.length - PENDING_OUTPUT_CHAR_CAP)],
      sequences: existingSequences ? [sequence] : null,
    };
  }
  let totalChars = chunk.length;
  for (const s of existing) totalChars += s.length;
  if (totalChars <= PENDING_OUTPUT_CHAR_CAP) {
    return {
      output: [...existing, chunk],
      sequences: existingSequences ? [...existingSequences, sequence] : null,
    };
  }
  // Evict oldest whole chunks first, then slice into the next chunk if we
  // still overflow. The resulting queue is always <= the cap.
  const next = existing.slice();
  const nextSequences = existingSequences?.slice() ?? null;
  let size = totalChars;
  while (next.length > 0 && size > PENDING_OUTPUT_CHAR_CAP) {
    const first = next[0]!;
    size -= first.length;
    next.shift();
    nextSequences?.shift();
  }
  if (size > PENDING_OUTPUT_CHAR_CAP && next.length > 0) {
    // Shouldn't happen with the loop above but keeps the invariant obvious.
    next.length = 0;
    if (nextSequences) nextSequences.length = 0;
    size = 0;
  }
  next.push(chunk);
  nextSequences?.push(sequence);
  return { output: next, sequences: nextSequences };
}

export function trimPendingOutput(existing: string[], chunk: string): string[] {
  return trimPendingOutputEntries(existing, chunk).output;
}

function trimPendingOutputWithSequences(
  existing: string[],
  existingSequences: number[],
  chunk: string,
  sequence: number,
): { output: string[]; sequences: number[] } {
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

  return {
    get tabs() { return tabs; },
    get activeTerminalID() { return activeTerminalID; },
    get drawerHeight() { return drawerHeight; },

    addTab(summary: TerminalSessionSummary): void {
      const existing = tabs.find((t) => t.terminalID === summary.terminalID);
      if (existing) {
        tabs = tabs.map((t) =>
          t.terminalID === summary.terminalID ? { ...t, summary } : t,
        );
        activeTerminalID = summary.terminalID;
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
      activeTerminalID = summary.terminalID;
    },

    removeTab(terminalID: string): void {
      const nextTabs = tabs.filter((t) => t.terminalID !== terminalID);
      tabs = nextTabs;
      pendingSequencesByTerminal.delete(terminalID);
      replayWatermarkByTerminal.delete(terminalID);
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

    appendOutput(terminalID: string, data: string, sequence = 0): void {
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

    drainOutput(terminalID: string): string[] {
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

      const nextOutput: string[] = [];
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

    clear(): void {
      tabs = [];
      activeTerminalID = null;
      pendingSequencesByTerminal.clear();
      replayWatermarkByTerminal.clear();
    },
  };
}

export interface ThreadTerminalStateHandle {
  readonly tabs: TerminalTabState[];
  readonly activeTerminalID: string | null;
  readonly drawerHeight: number;
  addTab(summary: TerminalSessionSummary): void;
  removeTab(terminalID: string): void;
  setActive(terminalID: string): void;
  appendOutput(terminalID: string, data: string, sequence?: number): void;
  drainOutput(terminalID: string): string[];
  markReplayed(terminalID: string, fromSequence: number, throughSequence: number): void;
  updateSummary(summary: TerminalSessionSummary): void;
  setDrawerHeight(height: number): void;
  clear(): void;
}

export const TERMINAL_DRAWER_LIMITS = {
  min: MIN_DRAWER_HEIGHT,
  max: MAX_DRAWER_HEIGHT,
  default: DEFAULT_DRAWER_HEIGHT,
};

const terminalStatesByThread = new Map<string, ThreadTerminalStateHandle>();

export function getThreadTerminalState(threadID: string): ThreadTerminalStateHandle {
  const key = threadID || '__unbound__';
  const existing = terminalStatesByThread.get(key);
  if (existing) return existing;
  const handle = createThreadTerminalState();
  terminalStatesByThread.set(key, handle);
  evictEmptyTerminalStates();
  return handle;
}

export function releaseThreadTerminalState(threadID: string): void {
  terminalStatesByThread.delete(threadID || '__unbound__');
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

// Module-level terminal-focus registry. The keybindings dispatcher reads
// `getTerminalFocused()` via makeCommandContext to gate `terminalFocus`-scoped
// chords. TerminalBody registers/deregisters via notifyTerminalFocus — the
// counter tolerates multiple mounts (e.g. keyed remount when swapping active
// tab) without flipping the flag prematurely.
let focusedTerminals = $state(0);

export function getTerminalFocused(): boolean {
  return focusedTerminals > 0;
}

export function notifyTerminalFocus(focused: boolean): void {
  if (focused) {
    focusedTerminals += 1;
  } else {
    focusedTerminals = Math.max(0, focusedTerminals - 1);
  }
}

/** Test helper — bring the registry back to zero between suites. */
export function resetTerminalFocusForTest(): void {
  focusedTerminals = 0;
}
