import type { TerminalSessionSummary } from '../../types/terminal';

export interface TerminalTabState {
  /** Wails-issued ID for the underlying PTY session. */
  terminalID: string;
  /** Pre-output summary from OpenTerminal/RestartTerminal/ListTerminals. */
  summary: TerminalSessionSummary;
  /** Incoming output events queue while the xterm instance is mounting. */
  pendingOutput: string[];
  /** Non-zero after a terminal:exit event landed. */
  exitCode: number | null;
  exitReason: string | null;
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
 * Creates a reactive state container for a thread's terminal drawer. Each
 * thread owns one of these; the drawer component reads/mutates through the
 * returned handle.
 */
export function createThreadTerminalState(): ThreadTerminalStateHandle {
  let tabs: TerminalTabState[] = $state([]);
  let activeTerminalID: string | null = $state(null);
  let drawerHeight: number = $state(DEFAULT_DRAWER_HEIGHT);

  return {
    get tabs() { return tabs; },
    get activeTerminalID() { return activeTerminalID; },
    get drawerHeight() { return drawerHeight; },

    addTab(summary: TerminalSessionSummary): void {
      tabs = [
        ...tabs,
        {
          terminalID: summary.terminalID,
          summary,
          pendingOutput: [],
          exitCode: null,
          exitReason: null,
        },
      ];
      activeTerminalID = summary.terminalID;
    },

    removeTab(terminalID: string): void {
      const nextTabs = tabs.filter((t) => t.terminalID !== terminalID);
      tabs = nextTabs;
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

    appendOutput(terminalID: string, data: string): void {
      tabs = tabs.map((t) =>
        t.terminalID === terminalID
          ? { ...t, pendingOutput: [...t.pendingOutput, data] }
          : t,
      );
    },

    drainOutput(terminalID: string): string[] {
      const match = tabs.find((t) => t.terminalID === terminalID);
      if (!match || match.pendingOutput.length === 0) return [];
      const drained = match.pendingOutput;
      tabs = tabs.map((t) =>
        t.terminalID === terminalID ? { ...t, pendingOutput: [] } : t,
      );
      return drained;
    },

    markExit(terminalID: string, code: number, reason: string): void {
      tabs = tabs.map((t) =>
        t.terminalID === terminalID
          ? {
              ...t,
              exitCode: code,
              exitReason: reason,
              summary: { ...t.summary, running: false, exitCode: code, exitReason: reason },
            }
          : t,
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
  appendOutput(terminalID: string, data: string): void;
  drainOutput(terminalID: string): string[];
  markExit(terminalID: string, code: number, reason: string): void;
  updateSummary(summary: TerminalSessionSummary): void;
  setDrawerHeight(height: number): void;
  clear(): void;
}

export const TERMINAL_DRAWER_LIMITS = {
  min: MIN_DRAWER_HEIGHT,
  max: MAX_DRAWER_HEIGHT,
  default: DEFAULT_DRAWER_HEIGHT,
};

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
