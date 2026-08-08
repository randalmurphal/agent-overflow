// SessionImportModal contract (the catalogue surface + the run it hosts):
//   - the four empty/error states are distinct, not collapsed into one
//     "nothing here" branch: all providers down, one provider down, healthy
//     but nothing to import, and healthy but filtered to nothing
//   - select-all is tri-state over the VISIBLE rows and never discards a
//     selection made under a different filter
//   - Esc is refused while a run is in flight (the store owns the guard, and
//     the modal has no second `open` to disagree with it)
//   - a run freezes every input on the surface, Cancel included
//   - one morphing primary: "Import all (N)" over the filtered set until
//     something is selected, then "Import (n)" over the selection, and
//     "Retry failed (n)" once a run has left failures on the rows
//   - the progress strip reports the run and can stop it; a stopped run
//     leaves the rows it never reached unstamped
//
// The store is driven for real, over the binding mocks one layer deeper —
// mocking a dependency of a `.svelte.ts` module is unreliable
// (frontend/AGENTS.md).

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import SessionImportModal from './SessionImportModal.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { setViewOnlySessionFromBootstrap } from '../../transport/runMode';
import {
  applyImportProgress,
  getImportSelection,
  isSessionImportOpen,
  loadImportCatalog,
  openSessionImport,
  resetSessionImportForTest,
  setProviderFilter,
  startImport,
  toggleRow,
} from '../../stores/sessionImport.svelte';
import type {
  ImportProviderStatus,
  ImportRunHandle,
  ImportScanResult,
  ImportableSession,
} from '../../types/sessionImport';

function row(id: string, extra: Partial<ImportableSession> = {}): ImportableSession {
  return {
    id,
    provider: id.startsWith('codex') ? 'codex' : 'claude',
    sessionId: id,
    title: `Session ${id}`,
    projectPath: '/repos/alpha',
    projectId: 'p-alpha',
    projectLabel: 'alpha',
    createdAt: 1,
    lastActivityAt: Date.now(),
    sizeBytes: 2048,
    branchCount: 1,
    subagentCount: 0,
    sourcePath: `/home/u/.claude/${id}.jsonl`,
    knownProject: true,
    ...extra,
  };
}

const HEALTHY: ImportProviderStatus[] = [
  { provider: 'claude', available: true, error: '', skippedCount: 0 },
  { provider: 'codex', available: true, error: '', skippedCount: 0 },
];

function scan(
  rows: ImportableSession[],
  providers: ImportProviderStatus[] = HEALTHY,
): ImportScanResult {
  return { providers, rows, scannedAt: 1_700_000_000_000 };
}

function installApi(
  result: ImportScanResult | (() => Promise<ImportScanResult>),
  handle: ImportRunHandle = { importId: 'run-1', total: 2 },
) {
  const listImportableSessions = setBindingMock(
    'ListImportableSessions',
    (typeof result === 'function' ? result : async () => result) as never,
  );
  const importSessions = setBindingMock('ImportSessions', (async () => handle) as never);
  setBindingMock('CancelSessionImport', (async () => {}) as never);
  return { listImportableSessions, importSessions };
}

/**
 * Load the catalogue BEFORE mounting so the modal's load-on-open effect hits
 * the store's already-ready short circuit. That keeps every assertion below
 * about rendering rather than about scan timing.
 */
async function mountWith(
  rows: ImportableSession[],
  providers: ImportProviderStatus[] = HEALTHY,
) {
  installApi(scan(rows, providers));
  await loadImportCatalog();
  openSessionImport();
  const view = render(SessionImportModal);
  await settle();
  return view;
}

async function settle(): Promise<void> {
  await Promise.resolve();
  await tick();
  await tick();
}

beforeEach(() => {
  resetSessionImportForTest();
  // A terminal progress frame resyncs the sidebar through the real
  // projection path; give it empty answers rather than "no mock" throws.
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListProjects', async () => []);
});

afterEach(() => {
  setViewOnlySessionFromBootstrap(false);
  resetSessionImportForTest();
});

describe('empty and error states', () => {
  it('all providers unavailable: one alert naming every provider and its error', async () => {
    const { getByTestId, queryByTestId } = await mountWith([], [
      { provider: 'claude', available: false, error: '~/.claude is unreadable', skippedCount: 0 },
      { provider: 'codex', available: false, error: '~/.codex does not exist', skippedCount: 0 },
    ]);

    const alert = getByTestId('session-import-providers-unavailable');
    expect(alert.getAttribute('role')).toBe('alert');
    expect(alert.textContent).toContain('~/.claude is unreadable');
    expect(alert.textContent).toContain('~/.codex does not exist');
    // Nothing to filter or select, so the toolbar is not rendered either.
    expect(queryByTestId('session-import-toolbar')).toBeNull();
    expect(queryByTestId('session-import-list')).toBeNull();
  });

  it('one provider errored: warning strip names it, the other provider still lists', async () => {
    const { getByTestId, queryByTestId } = await mountWith(
      [row('codex:a')],
      [
        { provider: 'claude', available: true, error: 'two session files were unreadable', skippedCount: 2 },
        ...HEALTHY.slice(1),
      ],
    );

    const strip = getByTestId('session-import-provider-warning-claude');
    expect(strip.textContent).toContain('Claude');
    expect(strip.textContent).toContain('two session files were unreadable');
    expect(queryByTestId('session-import-providers-unavailable')).toBeNull();
    expect(getByTestId('session-import-row-codex:a')).toBeTruthy();
    // The skipped-file footnote is quiet but present, and names the provider.
    expect(getByTestId('session-import-skipped').textContent).toContain(
      'Claude: 2 unreadable files skipped',
    );
  });

  it('healthy with zero rows: says WHY there is nothing, not just that there is nothing', async () => {
    const { getByTestId, queryByTestId } = await mountWith([]);

    expect(getByTestId('session-import-empty').textContent).toContain(
      'No sessions to import — everything Agent Overflow can see is already here.',
    );
    expect(queryByTestId('session-import-no-matches')).toBeNull();
  });

  it('rows exist but filters match none: separate state with a clear-filters escape', async () => {
    const { getByTestId, queryByTestId } = await mountWith([row('claude:a')]);
    expect(getByTestId('session-import-list')).toBeTruthy();

    setProviderFilter('codex');
    await settle();

    expect(queryByTestId('session-import-empty')).toBeNull();
    expect(getByTestId('session-import-no-matches').textContent).toContain(
      'No sessions match these filters.',
    );

    await fireEvent.click(getByTestId('session-import-clear-filters'));
    await settle();
    expect(getByTestId('session-import-row-claude:a')).toBeTruthy();
  });

  it('shows the scan state while the catalog is still loading', async () => {
    let release: (result: ImportScanResult) => void = () => {};
    installApi(() => new Promise<ImportScanResult>((resolve) => (release = resolve)));
    openSessionImport();
    const { getByTestId, queryByTestId } = render(SessionImportModal);
    await settle();

    expect(getByTestId('session-import-loading').textContent).toContain(
      'Scanning Claude Code and Codex session files…',
    );
    expect(queryByTestId('session-import-toolbar')).toBeNull();

    release(scan([row('claude:a')]));
    await settle();
    expect(getByTestId('session-import-row-claude:a')).toBeTruthy();
  });

  it('view-only: the store refuses the scan and the modal renders the reason', async () => {
    setViewOnlySessionFromBootstrap(true);
    installApi(scan([row('claude:a')]));
    openSessionImport();
    const { getByTestId } = render(SessionImportModal);
    await settle();

    expect(getByTestId('session-import-error').textContent).toContain('only available on the local app');
    expect(getByTestId('session-import-confirm')).toBeDisabled();
  });
});

describe('select-all', () => {
  it('cycles none → all → none over the visible rows', async () => {
    const { getByTestId } = await mountWith([row('claude:a'), row('claude:b')]);
    const checkbox = getByTestId('session-import-select-all') as HTMLInputElement;

    expect(checkbox.dataset.state).toBe('none');
    await fireEvent.click(checkbox);
    await settle();
    expect([...getImportSelection()].sort()).toEqual(['claude:a', 'claude:b']);
    expect(checkbox.dataset.state).toBe('all');

    await fireEvent.click(checkbox);
    await settle();
    expect([...getImportSelection()]).toEqual([]);
    expect(checkbox.dataset.state).toBe('none');
  });

  it("reports 'some' once part of the visible set is selected", async () => {
    const { getByTestId } = await mountWith([row('claude:a'), row('claude:b')]);

    toggleRow('claude:a');
    await settle();
    expect((getByTestId('session-import-select-all') as HTMLInputElement).dataset.state).toBe('some');
  });

  it('only adds or removes the VISIBLE rows — a selection made under another filter survives', async () => {
    const { getByTestId } = await mountWith([row('claude:a'), row('codex:b')]);

    toggleRow('codex:b');
    setProviderFilter('claude');
    await settle();

    const checkbox = getByTestId('session-import-select-all');
    await fireEvent.click(checkbox);
    await settle();
    expect([...getImportSelection()].sort()).toEqual(['claude:a', 'codex:b']);

    await fireEvent.click(checkbox);
    await settle();
    expect([...getImportSelection()]).toEqual(['codex:b']);
  });
});

describe('an in-flight run', () => {
  async function mountAndStart() {
    const view = await mountWith([row('claude:a'), row('codex:b')]);
    await startImport(['claude:a', 'codex:b']);
    await settle();
    return view;
  }

  it('refuses Esc — the modal stays open and the store stays open with it', async () => {
    const { container, getByRole } = await mountAndStart();

    await fireEvent.keyDown(container.querySelector('[data-modal-backdrop]')!, { key: 'Escape' });
    await settle();

    expect(isSessionImportOpen()).toBe(true);
    expect(getByRole('dialog')).toBeTruthy();
  });

  it('refuses a backdrop click for the same reason', async () => {
    const { container, getByRole } = await mountAndStart();

    await fireEvent.click(container.querySelector('[data-modal-backdrop]')!);
    await settle();

    expect(isSessionImportOpen()).toBe(true);
    expect(getByRole('dialog')).toBeTruthy();
  });

  it('disables every input on the surface, Cancel included', async () => {
    const { getByTestId, getByLabelText } = await mountAndStart();

    expect(getByTestId('session-import-select-all')).toBeDisabled();
    expect(getByTestId('session-import-project-select')).toBeDisabled();
    expect(getByTestId('session-import-search')).toBeDisabled();
    expect(getByTestId('session-import-refresh')).toBeDisabled();
    expect(getByTestId('session-import-cancel')).toBeDisabled();
    expect(getByTestId('session-import-confirm')).toBeDisabled();
    for (const segment of getByLabelText('Provider filter').querySelectorAll('button')) {
      expect(segment).toBeDisabled();
    }
    expect(getByTestId('session-import-list').getAttribute('aria-disabled')).toBe('true');
  });

  it('clicking a row while the run holds the surface does not change the selection', async () => {
    const { getByTestId } = await mountAndStart();

    await fireEvent.click(getByTestId('session-import-row-claude:a'));
    await settle();
    expect([...getImportSelection()]).toEqual([]);
  });

  it('the primary button reports progress instead of an action', async () => {
    const { getByTestId } = await mountAndStart();
    expect(getByTestId('session-import-confirm').textContent).toContain('Importing 0 of 2…');
  });
});

describe('keyboard', () => {
  function activeId(view: { getByTestId: (id: string) => HTMLElement }): string | null {
    return view.getByTestId('session-import-list').getAttribute('aria-activedescendant');
  }

  it('arrows move the roving cursor while focus is still in the search box', async () => {
    const view = await mountWith([row('claude:a'), row('claude:b')]);
    const search = view.getByTestId('session-import-search');

    const first = activeId(view);
    expect(first).toMatch(/claude:a$/);
    // The cursor has to be announced from where focus actually is, so the
    // search box mirrors it and points aria-controls at the listbox.
    expect(search.getAttribute('aria-activedescendant')).toBe(first);
    expect(search.getAttribute('aria-controls')).toBe(
      view.getByTestId('session-import-list').getAttribute('id'),
    );

    await fireEvent.keyDown(search, { key: 'ArrowDown' });
    await settle();
    expect(activeId(view)).toMatch(/claude:b$/);
    expect(search.getAttribute('aria-activedescendant')).toBe(activeId(view));

    // Clamped at the end rather than wrapping — a bulk list that jumps back
    // to the top on an over-press loses the user's place.
    await fireEvent.keyDown(search, { key: 'ArrowDown' });
    await settle();
    expect(activeId(view)).toMatch(/claude:b$/);

    await fireEvent.keyDown(search, { key: 'ArrowUp' });
    await settle();
    expect(activeId(view)).toBe(first);
  });

  it('Space toggles the active row, but only outside a text field', async () => {
    const view = await mountWith([row('claude:a'), row('claude:b')]);

    await fireEvent.keyDown(view.getByTestId('session-import-search'), { key: ' ' });
    await settle();
    expect([...getImportSelection()]).toEqual([]);

    await fireEvent.keyDown(view.getByTestId('session-import-list'), { key: ' ' });
    await settle();
    expect([...getImportSelection()]).toEqual(['claude:a']);
  });

  it('mod+a takes the empty search box (where text select-all is a no-op) and selects the filtered rows', async () => {
    const view = await mountWith([row('claude:a'), row('codex:b')]);
    setProviderFilter('claude');
    await settle();

    await fireEvent.keyDown(view.getByTestId('session-import-search'), { key: 'a', metaKey: true });
    await settle();
    expect([...getImportSelection()]).toEqual(['claude:a']);
  });

  it('mod+a leaves a NON-empty search box to the platform', async () => {
    const view = await mountWith([row('claude:a')]);
    const search = view.getByTestId('session-import-search') as HTMLInputElement;

    await fireEvent.input(search, { target: { value: 'Session' } });
    await settle();

    await fireEvent.keyDown(search, { key: 'a', ctrlKey: true });
    await settle();
    expect([...getImportSelection()]).toEqual([]);
  });

  it('Enter runs the import', async () => {
    const { importSessions } = installApi(scan([row('claude:a')]));
    await loadImportCatalog();
    openSessionImport();
    const { getByTestId } = render(SessionImportModal);
    await settle();

    await fireEvent.keyDown(getByTestId('session-import-search'), { key: 'Enter' });
    await settle();
    expect(importSessions).toHaveBeenCalledWith({ ids: ['claude:a'] });
  });

  it('goes inert once a run holds the surface', async () => {
    const view = await mountWith([row('claude:a'), row('claude:b')]);
    await startImport(['claude:a']);
    await settle();

    await fireEvent.keyDown(view.getByTestId('session-import-list'), { key: 'ArrowDown' });
    await settle();
    expect(activeId(view)).toMatch(/claude:a$/);
  });
});

describe('the morphing primary button', () => {
  it('offers the filtered set when nothing is selected', async () => {
    const { getByTestId } = await mountWith([row('claude:a'), row('claude:b'), row('codex:c')]);
    expect(getByTestId('session-import-confirm').textContent).toContain('Import all (3)');

    setProviderFilter('codex');
    await settle();
    expect(getByTestId('session-import-confirm').textContent).toContain('Import all (1)');
  });

  it('switches to the selection, and keeps counting rows the filters now hide', async () => {
    const { getByTestId } = await mountWith([row('claude:a'), row('codex:b')]);

    toggleRow('codex:b');
    await settle();
    expect(getByTestId('session-import-confirm').textContent).toContain('Import (1)');

    // Filtering the selected row out of view must not quietly shrink what
    // the button would import.
    setProviderFilter('claude');
    await settle();
    expect(getByTestId('session-import-confirm').textContent).toContain('Import (1)');
    expect(getByTestId('session-import-summary').textContent).toContain('1 of 2 selected');
  });

  it('imports the filtered ids on "Import all" and the selection otherwise', async () => {
    const { importSessions } = installApi(scan([row('claude:a'), row('codex:b')]));
    await loadImportCatalog();
    openSessionImport();
    const { getByTestId } = render(SessionImportModal);
    await settle();

    await fireEvent.click(getByTestId('session-import-confirm'));
    await settle();
    expect(importSessions).toHaveBeenCalledWith({ ids: ['claude:a', 'codex:b'] });
  });

  it('is disabled when the filters leave nothing to import', async () => {
    const { getByTestId } = await mountWith([row('claude:a')]);
    setProviderFilter('codex');
    await settle();
    expect(getByTestId('session-import-confirm')).toBeDisabled();
    expect(getByTestId('session-import-confirm').textContent).toContain('Import all (0)');
  });
});

describe('the progress strip', () => {
  function frame(extra: Record<string, unknown> = {}) {
    return { importId: 'run-1', completed: 0, total: 2, ...extra } as Parameters<
      typeof applyImportProgress
    >[0];
  }

  async function mountAndStart() {
    const view = await mountWith([row('claude:a'), row('codex:b')]);
    await startImport(['claude:a', 'codex:b']);
    await settle();
    return view;
  }

  it('is absent until a run exists, then reports the run over a live list', async () => {
    const view = await mountWith([row('claude:a'), row('codex:b')]);
    expect(view.queryByTestId('session-import-progress')).toBeNull();

    await startImport(['claude:a', 'codex:b']);
    await settle();
    expect(view.getByTestId('session-import-progress-headline').textContent).toContain(
      'Importing 0 of 2',
    );
    // The list is never swapped out for a progress screen — per-row errors
    // have to be able to land on their own rows.
    expect(view.getByTestId('session-import-row-claude:a')).toBeTruthy();
  });

  it('stamps outcomes onto the rows they belong to and rolls the failures up', async () => {
    const view = await mountAndStart();

    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'unreadable rollout' }));
    await settle();

    expect(view.getByTestId('session-import-outcome-claude:a').textContent).toContain('✓');
    expect(view.getByTestId('session-import-outcome-codex:b').textContent).toContain(
      'unreadable rollout',
    );
    expect(view.getByTestId('session-import-progress-detail').textContent).toContain('1 failed');
  });

  it('renders a skipped row’s prose as information, not as a failure', async () => {
    const view = await mountAndStart();

    applyImportProgress(
      frame({ completed: 1, id: 'claude:a', status: 'skipped', error: 'already imported' }),
    );
    await settle();

    const stamp = view.getByTestId('session-import-outcome-claude:a');
    expect(stamp.textContent).toContain('already imported');
    expect(stamp.querySelector('.text-error')).toBeNull();
    expect(view.getByTestId('session-import-progress-detail').textContent).toContain('1 skipped');
  });

  it('stops the run through the backend and waits for its terminal frame', async () => {
    const view = await mountAndStart();
    // After mounting: installApi seeds a permissive CancelSessionImport, and
    // this is the handle the assertion needs.
    const cancel = setBindingMock('CancelSessionImport', (async () => {}) as never);

    await fireEvent.click(view.getByTestId('session-import-stop'));
    await settle();

    expect(cancel).toHaveBeenCalledWith('run-1');
    expect(view.getByTestId('session-import-stop')).toBeDisabled();
    expect(view.getByTestId('session-import-progress-headline').textContent).toContain('Stopping');
    // Still active: the surface stays frozen until the backend says it stopped.
    expect(view.getByTestId('session-import-cancel')).toBeDisabled();
  });

  it('leaves a stopped run half-done rather than pretending it finished', async () => {
    const view = await mountAndStart();

    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 1, done: true }));
    await settle();

    expect(isSessionImportOpen()).toBe(true);
    expect(view.getByTestId('session-import-progress-headline').textContent).toContain(
      'Stopped after 1 of 2',
    );
    // The row the run never reached carries no stamp at all.
    expect(view.queryByTestId('session-import-outcome-codex:b')).toBeNull();
    // …and the surface is usable again, with the CTA back to its normal meaning.
    expect(view.getByTestId('session-import-cancel')).not.toBeDisabled();
    expect(view.getByTestId('session-import-confirm').textContent).toContain('Import all (2)');
    expect(view.queryByTestId('session-import-stop')).toBeNull();
  });

  it('offers a retry over exactly the failed rows once the run settles', async () => {
    const view = await mountAndStart();
    const { importSessions } = installApi(scan([row('claude:a'), row('codex:b')]), {
      importId: 'run-2',
      total: 1,
    });

    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'failed', error: 'boom' }));
    applyImportProgress(frame({ completed: 2, done: true }));
    await settle();

    const confirm = view.getByTestId('session-import-confirm');
    expect(confirm.textContent).toContain('Retry failed (1)');

    await fireEvent.click(confirm);
    await settle();
    expect(importSessions).toHaveBeenCalledWith({ ids: ['codex:b'] });
  });

  it('closes itself when the run finishes clean', async () => {
    const view = await mountAndStart();

    applyImportProgress(frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }));
    applyImportProgress(frame({ completed: 2, id: 'codex:b', status: 'imported', threadIds: ['t2'] }));
    applyImportProgress(frame({ completed: 2, done: true }));
    await settle();

    expect(isSessionImportOpen()).toBe(false);
    expect(view.queryByRole('dialog')).toBeNull();
  });
});
