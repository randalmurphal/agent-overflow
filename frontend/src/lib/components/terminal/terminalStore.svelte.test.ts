import { describe, expect, it, beforeEach } from 'vitest';
import {
  createThreadTerminalState,
  getTerminalFocused,
  notifyTerminalFocus,
  resetTerminalFocusForTest,
  TERMINAL_DRAWER_LIMITS,
} from './terminalStore.svelte';
import type { TerminalSessionSummary } from '../../types/terminal';

function makeSummary(overrides: Partial<TerminalSessionSummary> = {}): TerminalSessionSummary {
  return {
    terminalID: overrides.terminalID ?? 'term-1',
    threadID: overrides.threadID ?? 'thread-1',
    shell: overrides.shell ?? '/bin/bash',
    cwd: overrides.cwd ?? '/tmp',
    rows: overrides.rows ?? 24,
    cols: overrides.cols ?? 80,
    pid: overrides.pid ?? 1234,
    startedAt: overrides.startedAt ?? 0,
    running: overrides.running ?? true,
    exitCode: overrides.exitCode ?? 0,
    exitReason: overrides.exitReason ?? '',
  };
}

describe('ThreadTerminalState', () => {
  it('adds a tab and marks it active', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    expect(s.tabs).toHaveLength(1);
    expect(s.activeTerminalID).toBe('a');
  });

  it('appends and drains output for the matching tab', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.appendOutput('a', 'hello ');
    s.appendOutput('a', 'world');
    expect(s.tabs[0]!.pendingOutput).toEqual(['hello ', 'world']);
    const drained = s.drainOutput('a');
    expect(drained).toEqual(['hello ', 'world']);
    expect(s.tabs[0]!.pendingOutput).toEqual([]);
  });

  it('returns empty list when draining an unknown terminal', () => {
    const s = createThreadTerminalState();
    expect(s.drainOutput('nope')).toEqual([]);
  });

  it('removes a tab and promotes another tab to active', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.addTab(makeSummary({ terminalID: 'b' }));
    expect(s.activeTerminalID).toBe('b');
    s.removeTab('b');
    expect(s.tabs.map((t) => t.terminalID)).toEqual(['a']);
    expect(s.activeTerminalID).toBe('a');
  });

  it('clears the active terminal when the last tab closes', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'only' }));
    s.removeTab('only');
    expect(s.tabs).toHaveLength(0);
    expect(s.activeTerminalID).toBeNull();
  });

  it('ignores setActive for unknown terminals', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.setActive('missing');
    expect(s.activeTerminalID).toBe('a');
  });

  it('marks a tab as exited and mirrors the exit code into the summary', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.markExit('a', 137, 'signal:SIGKILL');
    expect(s.tabs[0]!.exitCode).toBe(137);
    expect(s.tabs[0]!.exitReason).toBe('signal:SIGKILL');
    expect(s.tabs[0]!.summary.running).toBe(false);
    expect(s.tabs[0]!.summary.exitCode).toBe(137);
  });

  it('clamps the drawer height between min and max', () => {
    const s = createThreadTerminalState();
    s.setDrawerHeight(-100);
    expect(s.drawerHeight).toBe(TERMINAL_DRAWER_LIMITS.min);
    s.setDrawerHeight(9999);
    expect(s.drawerHeight).toBe(TERMINAL_DRAWER_LIMITS.max);
  });

  it('updates the summary for a tab while preserving pending output', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a', rows: 24, cols: 80 }));
    s.appendOutput('a', 'xx');
    s.updateSummary(makeSummary({ terminalID: 'a', rows: 40, cols: 120 }));
    expect(s.tabs[0]!.summary.rows).toBe(40);
    expect(s.tabs[0]!.pendingOutput).toEqual(['xx']);
  });

  it('clear wipes tabs and active state', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.clear();
    expect(s.tabs).toEqual([]);
    expect(s.activeTerminalID).toBeNull();
  });
});

// --- Bug D5 regression: terminalFocus registry ---
describe('terminal focus registry', () => {
  beforeEach(() => {
    resetTerminalFocusForTest();
  });

  it('starts unfocused', () => {
    expect(getTerminalFocused()).toBe(false);
  });

  it('becomes focused on a single notifyTerminalFocus(true)', () => {
    notifyTerminalFocus(true);
    expect(getTerminalFocused()).toBe(true);
  });

  it('flips back to false when paired false notification arrives', () => {
    notifyTerminalFocus(true);
    notifyTerminalFocus(false);
    expect(getTerminalFocused()).toBe(false);
  });

  it('stays focused while at least one component holds focus (e.g. remount overlap)', () => {
    notifyTerminalFocus(true);
    notifyTerminalFocus(true);
    notifyTerminalFocus(false);
    // One component is still focused.
    expect(getTerminalFocused()).toBe(true);
    notifyTerminalFocus(false);
    expect(getTerminalFocused()).toBe(false);
  });

  it('never dips below zero (extra unfocus calls are tolerated)', () => {
    notifyTerminalFocus(false);
    notifyTerminalFocus(false);
    expect(getTerminalFocused()).toBe(false);
    notifyTerminalFocus(true);
    expect(getTerminalFocused()).toBe(true);
  });

  it('rapid toggling ends up in the expected state', () => {
    for (let i = 0; i < 20; i += 1) {
      notifyTerminalFocus(true);
      notifyTerminalFocus(false);
    }
    expect(getTerminalFocused()).toBe(false);
  });
});
