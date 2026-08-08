// The import store drives the real binding layer (setBindingMock one level
// deeper), never a hand-installed API object: mocking a dependency of a
// `.svelte.ts` module is unreliable (frontend/AGENTS.md), and the binding
// mocks are the seam every other store is tested through.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { setViewOnlySessionFromBootstrap } from '../transport/runMode';
import { getToasts, removeToast } from './toast.svelte';
import type {
  ImportScanResult,
  ImportableSession,
  SessionImportProgressEvent,
} from '../types/sessionImport';
import {
  applyImportProgress,
  closeSessionImport,
  getFailedImportIds,
  getImportRowResult,
  getImportRunCounts,
  getImportProjectFilter,
  getImportRows,
  getImportSelection,
  getSessionImportError,
  getSessionImportRun,
  getSessionImportStatus,
  isSessionImportOpen,
  loadImportCatalog,
  markImportConnectionLost,
  openSessionImport,
  resetSessionImportForTest,
  setProjectFilter,
  startImport,
  stopImport,
  toggleRow,
} from './sessionImport.svelte';

function row(id: string, extra: Partial<ImportableSession> = {}): ImportableSession {
  return {
    id,
    provider: 'claude',
    sessionId: id,
    title: `Session ${id}`,
    projectPath: '/repos/alpha',
    projectId: 'p-alpha',
    projectLabel: 'alpha',
    createdAt: 1,
    lastActivityAt: 2,
    sizeBytes: 10,
    branchCount: 0,
    subagentCount: 0,
    sourcePath: `/home/u/.claude/${id}.jsonl`,
    knownProject: true,
    ...extra,
  } as ImportableSession;
}

function scanResult(rows: ImportableSession[]): ImportScanResult {
  return {
    providers: [
      { provider: 'claude', available: true, error: '', skippedCount: 0 },
      { provider: 'codex', available: true, error: '', skippedCount: 0 },
    ],
    rows,
    scannedAt: 1_700_000_000_000,
  } as ImportScanResult;
}

/** Installs the two RPCs the store calls, defaulting both to success. */
function installBindings(
  overrides: {
    list?: (req: unknown) => Promise<ImportScanResult>;
    start?: (req: unknown) => Promise<{ importId: string; total: number }>;
    cancel?: (id: string) => Promise<void>;
  } = {},
) {
  const list = setBindingMock(
    'ListImportableSessions',
    (overrides.list ?? (async () => scanResult([row('claude:a'), row('codex:b')]))) as never,
  );
  const start = setBindingMock(
    'ImportSessions',
    (overrides.start ?? (async () => ({ importId: 'run-1', total: 2 }))) as never,
  );
  const cancel = setBindingMock('CancelSessionImport', (overrides.cancel ?? (async () => {})) as never);
  return { list, start, cancel };
}

function frame(extra: Partial<SessionImportProgressEvent> = {}): SessionImportProgressEvent {
  return { importId: 'run-1', completed: 0, total: 2, ...extra };
}

/** Puts a live run in place without asserting the start path. */
async function startRun(): Promise<void> {
  installBindings();
  await startImport(['claude:a', 'codex:b']);
}

function toastMessages(): string[] {
  return getToasts().map((t) => t.message);
}

function clearToasts(): void {
  for (const toast of [...getToasts()]) removeToast(toast.id);
}

beforeEach(() => {
  resetSessionImportForTest();
  clearToasts();
  // The `done` frame resyncs the sidebar through the real projection path
  // (mocking a dependency of a .svelte.ts module is unreliable), so give it
  // empty answers rather than "no mock installed" rejections.
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListProjects', async () => []);
});

afterEach(() => {
  setViewOnlySessionFromBootstrap(false);
  resetSessionImportForTest();
  clearToasts();
});

describe('catalog loading', () => {
  it('loads once and reuses the result until forced', async () => {
    const { list } = installBindings();

    await loadImportCatalog();
    expect(getSessionImportStatus()).toBe('ready');
    expect(getImportRows().map((r) => r.id)).toEqual(['claude:a', 'codex:b']);
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith({ forceRefresh: false });

    await loadImportCatalog();
    expect(list).toHaveBeenCalledTimes(1);

    await loadImportCatalog(true);
    expect(list).toHaveBeenCalledTimes(2);
    expect(list).toHaveBeenLastCalledWith({ forceRefresh: true });
  });

  it('joins a scan already in flight instead of starting a second one', async () => {
    const { list } = installBindings();
    const both = Promise.all([loadImportCatalog(), loadImportCatalog()]);
    expect(getSessionImportStatus()).toBe('loading');
    await both;
    expect(list).toHaveBeenCalledTimes(1);
  });

  it('surfaces a scan failure as user-facing state and drops the stale catalog', async () => {
    installBindings();
    await loadImportCatalog();
    expect(getImportRows()).toHaveLength(2);

    installBindings({
      list: async () => {
        throw new Error('claude home is unreadable');
      },
    });
    await loadImportCatalog(true);

    expect(getSessionImportStatus()).toBe('error');
    expect(getSessionImportError()).toBe('Claude home is unreadable.');
    expect(getImportRows()).toEqual([]);
  });

  it('drops selections and a project filter the new catalog no longer holds', async () => {
    installBindings();
    await loadImportCatalog();
    toggleRow('claude:a');
    toggleRow('codex:b');
    setProjectFilter('/repos/alpha');

    installBindings({
      list: async () => scanResult([row('codex:b', { projectPath: '/repos/beta' })]),
    });
    await loadImportCatalog(true);

    expect([...getImportSelection()]).toEqual(['codex:b']);
    expect(getImportProjectFilter()).toBeNull();
  });
});

describe('view-only sessions', () => {
  it('refuses to scan and says why', async () => {
    const { list } = installBindings();
    setViewOnlySessionFromBootstrap(true);

    await loadImportCatalog();

    expect(list).not.toHaveBeenCalled();
    expect(getSessionImportStatus()).toBe('error');
    expect(getSessionImportError()).toMatch(/only available on the local app/);
  });

  it('refuses to start a run and says why', async () => {
    const { start } = installBindings();
    setViewOnlySessionFromBootstrap(true);

    await startImport(['claude:a']);

    expect(start).not.toHaveBeenCalled();
    expect(getSessionImportRun()).toBeNull();
    expect(getSessionImportError()).toMatch(/only available on the local app/);
  });
});

describe('starting a run', () => {
  it('dedupes ids and adopts the backend total', async () => {
    const { start } = installBindings({
      start: async () => ({ importId: 'run-9', total: 5 }),
    });

    await startImport(['claude:a', 'claude:a', '', 'codex:b']);

    expect(start).toHaveBeenCalledWith({ ids: ['claude:a', 'codex:b'] });
    expect(getSessionImportRun()).toMatchObject({
      importId: 'run-9',
      total: 5,
      completed: 0,
      active: true,
      stopRequested: false,
    });
  });

  it('is a no-op for an empty selection', async () => {
    const { start } = installBindings();
    await startImport([]);
    expect(start).not.toHaveBeenCalled();
    expect(getSessionImportRun()).toBeNull();
  });

  it('refuses a second run while the accept round trip is still open', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const { start } = installBindings({
      start: async () => {
        await gate;
        return { importId: 'run-1', total: 1 };
      },
    });

    const first = startImport(['claude:a']);
    const second = startImport(['codex:b']);
    release?.();
    await Promise.all([first, second]);

    expect(start).toHaveBeenCalledTimes(1);
  });

  it('surfaces a refused run and leaves no run behind', async () => {
    installBindings({
      start: async () => {
        throw new Error('import already running');
      },
    });
    await startImport(['claude:a']);
    expect(getSessionImportRun()).toBeNull();
    expect(getSessionImportError()).toBe('Import already running.');
  });
});

describe('progress folding', () => {
  it('records a per-row outcome and advances completion', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1', 't2'] }));

    const run = getSessionImportRun();
    expect(run?.completed).toBe(1);
    expect(run?.results.get('claude:a')).toEqual({
      id: 'claude:a',
      status: 'imported',
      threadIds: ['t1', 't2'],
      error: '',
    });
  });

  it('never walks completion backwards when frames arrive out of order', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'imported' }));
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'failed', error: 'unreadable' }));

    const run = getSessionImportRun();
    expect(run?.completed).toBe(2);
    // The late frame's outcome is still recorded — only the counter is monotonic.
    expect(run?.results.get('claude:a')).toMatchObject({ status: 'failed', error: 'unreadable' });
  });

  it('is idempotent under a re-delivered frame', async () => {
    await startRun();
    const f = frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] });
    applyImportProgress(f);
    applyImportProgress(f);

    const run = getSessionImportRun();
    expect(run?.completed).toBe(1);
    expect(run?.results.size).toBe(1);
    expect(run?.results.get('claude:a')?.threadIds).toEqual(['t1']);
  });

  it('clamps a completion overshoot to the total', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 99, id: 'claude:a', status: 'imported' }));
    expect(getSessionImportRun()?.completed).toBe(2);
  });

  it('ignores frames after the terminal one', async () => {
    await startRun();
    openSessionImport();
    applyImportProgress(frame({ completed: 2, id: 'claude:a', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, done: true }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'late' }));

    expect(getSessionImportRun()?.results.has('codex:b')).toBe(false);
  });

  it('ignores frames belonging to another run', async () => {
    await startRun();
    applyImportProgress({ importId: 'run-other', completed: 2, total: 2, done: true });

    expect(getSessionImportRun()).toMatchObject({ active: true, completed: 0 });
  });

  it('adopts a total the backend revises upward', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 3, total: 6, id: 'claude:a', status: 'imported' }));
    expect(getSessionImportRun()).toMatchObject({ total: 6, completed: 3 });
  });

  it('drops a frame with an unrecognised status rather than stamping the row', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'exploded' as never }));

    const run = getSessionImportRun();
    expect(run?.results.size).toBe(0);
    expect(run?.completed).toBe(1);
  });
});

// The results map is mutated in place — a clone per frame is thousands of
// copies of a growing map on a real "Import all" — so the reactive signal is
// a version counter and the tally is maintained incrementally. These pin the
// bookkeeping that replaces the fold.
describe('per-row results and the tally', () => {
  it('bumps the version on every stamp so readers can gate on it', async () => {
    await startRun();
    expect(getSessionImportRun()?.resultsVersion).toBe(0);

    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    expect(getSessionImportRun()?.resultsVersion).toBe(1);
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'boom' }));
    expect(getSessionImportRun()?.resultsVersion).toBe(2);
  });

  it('reads one row through the accessor, and nothing for a row not reported', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'skipped', error: 'gone' }));

    expect(getImportRowResult('claude:a')).toMatchObject({ status: 'skipped', error: 'gone' });
    expect(getImportRowResult('codex:b')).toBeUndefined();
  });

  it('counts by status and sums the threads imported rows created', async () => {
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1', 't2'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, id: 'claude:c', status: 'skipped', error: 'already here' }));

    expect(getImportRunCounts()).toEqual({ imported: 1, failed: 1, skipped: 1, threads: 2 });
  });

  it('retracts the old contribution when a replayed frame restates a row', async () => {
    await startRun();
    const first = frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1', 't2'] });
    applyImportProgress(first);
    // Re-delivery of the same frame (a reconnect replays the channel).
    applyImportProgress(first);
    expect(getImportRunCounts()).toEqual({ imported: 1, failed: 0, skipped: 0, threads: 2 });

    // …and a genuine restatement moves the row between buckets rather than
    // counting it twice.
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'failed', error: 'boom' }));
    expect(getImportRunCounts()).toEqual({ imported: 0, failed: 1, skipped: 0, threads: 0 });
  });

  it('reports an all-zero tally with no run rather than making the caller null-check', () => {
    expect(getImportRunCounts()).toEqual({ imported: 0, failed: 0, skipped: 0, threads: 0 });
    expect(getImportRowResult('claude:a')).toBeUndefined();
  });
});

describe('a run that finishes clean', () => {
  it('closes the surface and reports the threads only the run could know', async () => {
    openSessionImport();
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1', 't2'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'imported', threadIds: ['t3'] }));
    applyImportProgress(frame({ completed: 2, done: true }));

    expect(isSessionImportOpen()).toBe(false);
    expect(getSessionImportRun()).toBeNull();
    expect(toastMessages()).toEqual(['Imported 2 sessions (3 threads).']);
  });

  it('names the skipped rows rather than counting them as imports', async () => {
    openSessionImport();
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'skipped', error: 'already imported' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    expect(toastMessages()).toEqual(['Imported 1 session (1 thread). 1 session skipped.']);
  });

  it('re-scans on the next load instead of re-offering what it just imported', async () => {
    openSessionImport();
    installBindings();
    await loadImportCatalog();
    const { list } = installBindings();
    await startImport(['claude:a']);
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'imported', threadIds: ['t2'] }));
    applyImportProgress(frame({ completed: 2, done: true }));

    // Reopening reuses a loaded catalogue by design — except this one, which
    // still lists the sessions the run consumed.
    await loadImportCatalog();
    expect(list).toHaveBeenCalledTimes(1);

    await loadImportCatalog();
    expect(list).toHaveBeenCalledTimes(1);
  });

  it('says so when everything was skipped instead of claiming an import', async () => {
    openSessionImport();
    await startRun();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'skipped', error: 'gone' }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'skipped', error: 'gone' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    expect(isSessionImportOpen()).toBe(false);
    expect(toastMessages()[0]).toMatch(/Nothing to import/);
  });
});

describe('a run that finishes with failures', () => {
  it('keeps the surface open with the stamps and offers exactly the failed rows', async () => {
    openSessionImport();
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a', 'codex:b']);
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'unreadable rollout' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    expect(isSessionImportOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);
    expect(getSessionImportRun()).toMatchObject({ active: false, completed: 2 });
    expect(getFailedImportIds()).toEqual(['codex:b']);
  });

  it('offers no retry mid-run — the failures are not final until the run is', async () => {
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a', 'codex:b']);
    applyImportProgress(frame({ completed: 1, id: 'codex:b', status: 'failed', error: 'boom' }));

    // The row is stamped, but the run may still retry it itself — and
    // walking the whole catalog on every frame is the fold the counts exist
    // to avoid.
    expect(getImportRowResult('codex:b')?.status).toBe('failed');
    expect(getFailedImportIds()).toEqual([]);

    applyImportProgress(frame({ completed: 2, done: true }));
    expect(getFailedImportIds()).toEqual(['codex:b']);
  });

  it('reports no failed ids for rows the catalog no longer holds', async () => {
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a']);
    applyImportProgress(frame({ completed: 1, id: 'gone-row', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    expect(getFailedImportIds()).toEqual([]);
  });

  it('retries by starting a fresh run over just those ids', async () => {
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a', 'codex:b']);
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'unreadable' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    const start = getBindingMock('ImportSessions');
    start?.mockClear();
    await startImport(getFailedImportIds());

    expect(start).toHaveBeenCalledWith({ ids: ['codex:b'] });
    // The new run starts clean: the previous stamps are not carried over.
    expect(getSessionImportRun()).toMatchObject({ active: true, completed: 0 });
    expect(getSessionImportRun()?.results.size).toBe(0);
  });
});

// A gap ends the run for the SURFACE and for nothing else: the backend keeps
// importing and keeps emitting, so the frames after one are ordinary live
// frames about a run that is still going. Ignoring them (the old `!active`
// cutoff) froze the modal on the last pre-gap count and threw away the very
// frame that finishes the run.
describe('a run whose frames were gapped', () => {
  it('keeps folding the live frames that follow and settles on the eventual done', async () => {
    openSessionImport();
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a', 'codex:b']);
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));

    markImportConnectionLost();
    expect(getSessionImportRun()).toMatchObject({
      active: false,
      terminal: false,
      connectionLost: true,
    });

    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'unreadable rollout' }));
    expect(getImportRowResult('codex:b')?.status).toBe('failed');
    expect(getImportRunCounts()).toEqual({ imported: 1, failed: 1, skipped: 0, threads: 1 });

    applyImportProgress(frame({ completed: 2, done: true }));

    // Settled, never summarised: the gap took an unknown number of outcomes
    // with it, so the surface stays open on what is known — and the retry
    // offers the failures the run actually reported, not the pre-gap subset.
    expect(getSessionImportRun()).toMatchObject({
      active: false,
      terminal: true,
      connectionLost: true,
      completed: 2,
    });
    expect(isSessionImportOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);
    expect(getFailedImportIds()).toEqual(['codex:b']);
  });

  it('does not close or toast over a finish that only LOOKS clean', async () => {
    openSessionImport();
    await startRun();
    markImportConnectionLost();
    applyImportProgress(frame({ completed: 2, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, done: true }));

    // Nothing failed and the bar reached its total, but the counts are a
    // floor rather than the answer — claiming "Imported 1 session" would be
    // a claim this client cannot make.
    expect(isSessionImportOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);
  });

  it('ignores everything after the terminal frame, gap or no gap', async () => {
    installBindings();
    await loadImportCatalog();
    await startImport(['claude:a', 'codex:b']);
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    applyImportProgress(frame({ completed: 2, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));

    expect(getImportRowResult('claude:a')).toBeUndefined();
    expect(getImportRunCounts()).toEqual({ imported: 0, failed: 1, skipped: 0, threads: 0 });
  });
});

describe('stopping a run', () => {
  it('asks the backend and waits for its terminal frame rather than ending locally', async () => {
    openSessionImport();
    await startRun();
    const { cancel } = installBindings();

    await stopImport();

    expect(cancel).toHaveBeenCalledWith('run-1');
    expect(getSessionImportRun()).toMatchObject({ active: true, stopRequested: true });
  });

  it('only asks once', async () => {
    await startRun();
    const { cancel } = installBindings();
    await stopImport();
    await stopImport();
    expect(cancel).toHaveBeenCalledTimes(1);
  });

  it('re-arms and surfaces the error when the cancel call is refused', async () => {
    await startRun();
    installBindings({
      cancel: async () => {
        throw new Error('no import run is in progress');
      },
    });

    await stopImport();

    expect(getSessionImportRun()).toMatchObject({ active: true, stopRequested: false });
    expect(getSessionImportError()).toBe('No import run is in progress.');
  });

  it('leaves the short terminal frame short — the surface stays open and unstamped rows stay blank', async () => {
    openSessionImport();
    await startRun();
    installBindings();
    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    await stopImport();
    applyImportProgress(frame({ completed: 1, done: true }));

    const run = getSessionImportRun();
    expect(run).toMatchObject({ active: false, completed: 1, total: 2, stopRequested: false });
    expect(run?.results.has('codex:b')).toBe(false);
    expect(isSessionImportOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);
    // Nothing failed, so the CTA goes back to being a normal import.
    expect(getFailedImportIds()).toEqual([]);
  });
});

describe('closing', () => {
  it('is refused while a run is active, whatever asked', async () => {
    openSessionImport();
    await startRun();

    closeSessionImport();
    expect(isSessionImportOpen()).toBe(true);
    expect(getSessionImportRun()?.active).toBe(true);
  });

  it('closes after a failed run, dropping stamps and selection', async () => {
    openSessionImport();
    await startRun();
    toggleRow('claude:a');
    applyImportProgress(frame({ completed: 2, id: 'claude:a', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, done: true }));

    closeSessionImport();
    expect(isSessionImportOpen()).toBe(false);
    expect(getSessionImportRun()).toBeNull();
    expect(getImportSelection().size).toBe(0);
  });

  it('is closable again after a transport gap proved frames were lost', async () => {
    openSessionImport();
    await startRun();
    // Only the GAP signal lands here — a socket blip is recoverable (the
    // transport replays the channel) and deliberately does not end a run.
    markImportConnectionLost();

    expect(getSessionImportRun()).toMatchObject({ active: false, connectionLost: true });
    // A lost connection is not a success: nothing closes itself and nothing
    // claims an import happened.
    expect(isSessionImportOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);

    closeSessionImport();
    expect(isSessionImportOpen()).toBe(false);
  });
});

describe('selection', () => {
  it('toggles copy-on-write so readers see a new set', async () => {
    installBindings();
    await loadImportCatalog();

    const before = getImportSelection();
    toggleRow('claude:a');
    const after = getImportSelection();
    expect(before).not.toBe(after);
    expect([...after]).toEqual(['claude:a']);

    toggleRow('claude:a');
    expect(getImportSelection().size).toBe(0);
  });
});

describe('the unwired case', () => {
  it('fails loudly when the binding itself is missing', async () => {
    // No mock installed for ListImportableSessions: the binding mock rejects
    // rather than resolving empty, and the store turns that into visible
    // state instead of an empty catalogue that looks like "nothing to do".
    vi.spyOn(console, 'error').mockImplementation(() => {});
    await loadImportCatalog();
    expect(getSessionImportStatus()).toBe('error');
    expect(getSessionImportError()).toMatch(/ListImportableSessions/);
  });
});
