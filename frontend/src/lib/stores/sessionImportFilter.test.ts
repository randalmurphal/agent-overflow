import { describe, expect, it } from 'vitest';
import type { ImportProviderStatus, ImportableSession } from '../types/sessionImport';
import {
  buildProjectGroups,
  filterImportRows,
  importSurface,
  selectAllState,
  selectionSummary,
  surfaceHasCatalog,
} from './sessionImportFilter';

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
    sizeBytes: 100,
    branchCount: 1,
    subagentCount: 0,
    sourcePath: `/home/u/.claude/${id}.jsonl`,
    knownProject: true,
    ...extra,
  };
}

const CATALOG: ImportableSession[] = [
  row('claude:a', { title: 'Fix the parser', gitBranch: 'feat/parser' }),
  row('claude:b', { title: 'Nightly sweep', projectPath: '/repos/beta', projectLabel: 'beta' }),
  row('codex:c', {
    provider: 'codex',
    title: 'Rewrite the router',
    gitBranch: 'chore/router',
  }),
  row('codex:d', {
    provider: 'codex',
    title: 'Ship the parser docs',
    projectPath: '/repos/beta',
    projectLabel: 'beta',
  }),
];

const NO_FILTERS = { providerFilter: 'all', projectFilter: null, query: '' } as const;

describe('filterImportRows', () => {
  it('passes every row through when nothing is filtered', () => {
    expect(filterImportRows(CATALOG, NO_FILTERS).map((r) => r.id)).toEqual([
      'claude:a',
      'claude:b',
      'codex:c',
      'codex:d',
    ]);
  });

  it('applies provider, project and query together', () => {
    const got = filterImportRows(CATALOG, {
      providerFilter: 'codex',
      projectFilter: '/repos/beta',
      query: 'parser',
    });
    expect(got.map((r) => r.id)).toEqual(['codex:d']);
  });

  it('matches title, project label and git branch case-insensitively', () => {
    const byTitle = filterImportRows(CATALOG, { ...NO_FILTERS, query: '  PARSER ' });
    expect(byTitle.map((r) => r.id)).toEqual(['claude:a', 'codex:d']);

    const byProject = filterImportRows(CATALOG, { ...NO_FILTERS, query: 'Beta' });
    expect(byProject.map((r) => r.id)).toEqual(['claude:b', 'codex:d']);

    const byBranch = filterImportRows(CATALOG, { ...NO_FILTERS, query: 'chore/' });
    expect(byBranch.map((r) => r.id)).toEqual(['codex:c']);
  });

  it('does not match on fields the row never shows', () => {
    expect(filterImportRows(CATALOG, { ...NO_FILTERS, query: '.jsonl' })).toEqual([]);
    expect(filterImportRows(CATALOG, { ...NO_FILTERS, query: 'p-alpha' })).toEqual([]);
  });

  it('keeps catalog order', () => {
    const got = filterImportRows(CATALOG, { ...NO_FILTERS, providerFilter: 'claude' });
    expect(got.map((r) => r.id)).toEqual(['claude:a', 'claude:b']);
  });
});

describe('buildProjectGroups', () => {
  it('lists every project regardless of the provider filter, and sorts by label', () => {
    const groups = buildProjectGroups(CATALOG, { providerFilter: 'codex', query: '' });
    expect(groups.map((g) => g.path)).toEqual(['/repos/alpha', '/repos/beta']);
    expect(groups.map((g) => g.label)).toEqual(['alpha', 'beta']);
  });

  it('counts only rows surviving the provider filter', () => {
    const all = buildProjectGroups(CATALOG, { providerFilter: 'all', query: '' });
    expect(all.map((g) => g.count)).toEqual([2, 2]);

    const claude = buildProjectGroups(CATALOG, { providerFilter: 'claude', query: '' });
    expect(claude.map((g) => [g.path, g.count])).toEqual([
      ['/repos/alpha', 1],
      ['/repos/beta', 1],
    ]);
  });

  it('keeps a group whose count the filters drove to zero', () => {
    const groups = buildProjectGroups(CATALOG, { providerFilter: 'claude', query: 'router' });
    expect(groups.map((g) => [g.path, g.count])).toEqual([
      ['/repos/alpha', 0],
      ['/repos/beta', 0],
    ]);
  });

  it('ignores the project filter entirely — the menu must not shrink to its own pick', () => {
    const groups = buildProjectGroups(CATALOG, { providerFilter: 'all', query: 'sweep' });
    expect(groups.map((g) => [g.path, g.count])).toEqual([
      ['/repos/alpha', 0],
      ['/repos/beta', 1],
    ]);
  });

  it('orders ties by path so scan order cannot reshuffle the menu', () => {
    const dupes = [
      row('x', { projectPath: '/repos/z', projectLabel: 'same' }),
      row('y', { projectPath: '/repos/a', projectLabel: 'Same' }),
    ];
    expect(buildProjectGroups(dupes, { providerFilter: 'all', query: '' }).map((g) => g.path))
      .toEqual(['/repos/a', '/repos/z']);
  });
});

describe('selectionSummary', () => {
  it('sums size over the whole catalog, not the filtered view', () => {
    const catalog = [
      row('a', { sizeBytes: 10 }),
      row('b', { sizeBytes: 25 }),
      row('c', { sizeBytes: 99 }),
    ];
    expect(selectionSummary(catalog, new Set(['a', 'b']))).toEqual({ count: 2, bytes: 35 });
  });

  // branchCount is 0 on every Claude row (NOT DETERMINED, not "no threads"),
  // so nothing derived from it may reach the footer — see the type's comment.
  it('reports no thread figure, whatever branchCount says', () => {
    const catalog = [row('a', { branchCount: 0 }), row('b', { branchCount: 1 })];
    expect(Object.keys(selectionSummary(catalog, new Set(['a', 'b']))).sort()).toEqual([
      'bytes',
      'count',
    ]);
  });

  it('ignores selected ids the catalog no longer holds', () => {
    expect(selectionSummary(CATALOG, new Set(['gone']))).toEqual({ count: 0, bytes: 0 });
  });
});

describe('selectAllState', () => {
  it('reports none for an empty view even when other rows are selected', () => {
    expect(selectAllState([], new Set(['claude:a']))).toBe('none');
  });

  it('walks none → some → all over the visible rows', () => {
    const visible = filterImportRows(CATALOG, { ...NO_FILTERS, providerFilter: 'claude' });
    expect(selectAllState(visible, new Set())).toBe('none');
    expect(selectAllState(visible, new Set(['claude:a']))).toBe('some');
    expect(selectAllState(visible, new Set(['claude:a', 'claude:b']))).toBe('all');
  });

  it('ignores selections outside the visible rows', () => {
    const visible = filterImportRows(CATALOG, { ...NO_FILTERS, providerFilter: 'claude' });
    expect(selectAllState(visible, new Set(['codex:c']))).toBe('none');
    expect(selectAllState(visible, new Set(['claude:a', 'claude:b', 'codex:c']))).toBe('all');
  });
});

describe('importSurface', () => {
  const HEALTHY: ImportProviderStatus[] = [
    { provider: 'claude', available: true, error: '', skippedCount: 0 },
    { provider: 'codex', available: true, error: '', skippedCount: 0 },
  ];

  function surface(over: Partial<Parameters<typeof importSurface>[0]> = {}) {
    return importSurface({
      status: 'ready',
      providers: HEALTHY,
      rowCount: 2,
      filteredCount: 2,
      ...over,
    });
  }

  it('treats an unscanned catalog as loading, not as empty', () => {
    expect(surface({ status: 'idle' })).toBe('loading');
    expect(surface({ status: 'loading' })).toBe('loading');
  });

  it('reports a failed scan before anything about rows', () => {
    expect(surface({ status: 'error', rowCount: 0, filteredCount: 0 })).toBe('error');
  });

  it('reports every provider being unreadable as its own state', () => {
    expect(
      surface({
        providers: HEALTHY.map((p) => ({ ...p, available: false })),
        rowCount: 0,
        filteredCount: 0,
      }),
    ).toBe('unavailable');
  });

  it('does NOT read an empty provider list as every provider being broken', () => {
    // The contract guarantees one entry per provider; none at all means the
    // scan reported nothing, which is an empty catalog.
    expect(surface({ providers: [], rowCount: 0, filteredCount: 0 })).toBe('empty');
  });

  it('separates "nothing to import" from "nothing matches these filters"', () => {
    expect(surface({ rowCount: 0, filteredCount: 0 })).toBe('empty');
    expect(surface({ rowCount: 5, filteredCount: 0 })).toBe('no-matches');
    expect(surface({ rowCount: 5, filteredCount: 1 })).toBe('rows');
  });

  it('keeps the toolbar for every catalog-shaped state, so no-matches is escapable', () => {
    expect(surfaceHasCatalog('rows')).toBe(true);
    expect(surfaceHasCatalog('empty')).toBe(true);
    expect(surfaceHasCatalog('no-matches')).toBe(true);
    expect(surfaceHasCatalog('loading')).toBe(false);
    expect(surfaceHasCatalog('error')).toBe(false);
    expect(surfaceHasCatalog('unavailable')).toBe(false);
  });
});
