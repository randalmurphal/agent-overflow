import { describe, expect, it } from 'vitest';
import type { ImportProviderStatus, ImportableSession } from '../types/sessionImport';
import {
  buildProjectGroups,
  countAlreadyRanRows,
  filterImportRows,
  importSurface,
  projectGroupKey,
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
    subagentCount: 0,
    sourcePath: `/home/u/.claude/${id}.jsonl`,
    knownProject: true,
    origin: '',
    ranInAgentOverflow: false,
    ...extra,
  };
}

/** A row with no AO project: keyed and labelled by its own cwd. */
function unknownProjectRow(id: string, path: string, extra: Partial<ImportableSession> = {}) {
  return row(id, {
    projectPath: path,
    projectId: '',
    projectLabel: path.slice(path.lastIndexOf('/') + 1),
    knownProject: false,
    ...extra,
  });
}

const CATALOG: ImportableSession[] = [
  row('claude:a', { title: 'Fix the parser', gitBranch: 'feat/parser' }),
  row('claude:b', {
    title: 'Nightly sweep',
    projectPath: '/repos/beta',
    projectId: 'p-beta',
    projectLabel: 'beta',
  }),
  row('codex:c', {
    provider: 'codex',
    title: 'Rewrite the router',
    gitBranch: 'chore/router',
  }),
  row('codex:d', {
    provider: 'codex',
    title: 'Ship the parser docs',
    projectPath: '/repos/beta',
    projectId: 'p-beta',
    projectLabel: 'beta',
  }),
];

const NO_FILTERS = {
  providerFilter: 'all',
  projectFilter: null,
  query: '',
  showAlreadyRan: false,
} as const;

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
      ...NO_FILTERS,
      providerFilter: 'codex',
      projectFilter: 'p-beta',
      query: 'parser',
    });
    expect(got.map((r) => r.id)).toEqual(['codex:d']);
  });

  it('matches a project filter against every cwd that resolves to it', () => {
    // A repo root and a subdirectory the agent happened to run in are ONE
    // project, and picking it has to bring both cwds' rows.
    const catalog = [
      row('root', { projectPath: '/repos/alpha' }),
      row('sub', { projectPath: '/repos/alpha/frontend', projectLabel: 'alpha' }),
      unknownProjectRow('stranger', '/repos/alpha-ish'),
    ];
    const got = filterImportRows(catalog, { ...NO_FILTERS, projectFilter: 'p-alpha' });
    expect(got.map((r) => r.id)).toEqual(['root', 'sub']);
  });

  it('keys a project-less row by its path, which is what the filter stores', () => {
    const orphan = unknownProjectRow('orphan', '/tmp/scratch');
    expect(projectGroupKey(orphan)).toBe('/tmp/scratch');
    expect(projectGroupKey(row('known'))).toBe('p-alpha');
    expect(
      filterImportRows([...CATALOG, orphan], {
        ...NO_FILTERS,
        projectFilter: '/tmp/scratch',
      }).map((r) => r.id),
    ).toEqual(['orphan']);
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

  it('withholds rows Agent Overflow itself produced until they are asked for', () => {
    const catalog = [row('mine'), row('ao', { ranInAgentOverflow: true })];
    expect(filterImportRows(catalog, NO_FILTERS).map((r) => r.id)).toEqual(['mine']);
    expect(
      filterImportRows(catalog, { ...NO_FILTERS, showAlreadyRan: true }).map((r) => r.id),
    ).toEqual(['mine', 'ao']);
  });

  it('applies the other filters to the revealed rows like any other row', () => {
    const catalog = [
      row('ao:claude', { ranInAgentOverflow: true }),
      row('ao:codex', { provider: 'codex', ranInAgentOverflow: true }),
    ];
    const got = filterImportRows(catalog, {
      ...NO_FILTERS,
      showAlreadyRan: true,
      providerFilter: 'codex',
    });
    expect(got.map((r) => r.id)).toEqual(['ao:codex']);
  });
});

describe('countAlreadyRanRows', () => {
  const CATALOG_WITH_AO: ImportableSession[] = [
    row('mine'),
    row('ao:a', { ranInAgentOverflow: true, title: 'Fix the parser' }),
    row('ao:b', { provider: 'codex', ranInAgentOverflow: true, title: 'Nightly sweep' }),
    row('ao:c', {
      ranInAgentOverflow: true,
      projectPath: '/repos/beta',
      projectId: 'p-beta',
      projectLabel: 'beta',
    }),
  ];

  it('counts the withheld rows under the CURRENT provider, project and query', () => {
    expect(countAlreadyRanRows(CATALOG_WITH_AO, NO_FILTERS)).toBe(3);
    expect(countAlreadyRanRows(CATALOG_WITH_AO, { ...NO_FILTERS, providerFilter: 'codex' })).toBe(1);
    expect(countAlreadyRanRows(CATALOG_WITH_AO, { ...NO_FILTERS, projectFilter: 'p-beta' })).toBe(1);
    expect(countAlreadyRanRows(CATALOG_WITH_AO, { ...NO_FILTERS, query: 'parser' })).toBe(1);
  });

  it('answers the same with the toggle on — it is what turning it off would remove', () => {
    expect(countAlreadyRanRows(CATALOG_WITH_AO, { ...NO_FILTERS, showAlreadyRan: true })).toBe(3);
  });

  it('is zero for a catalog with nothing to withhold', () => {
    expect(countAlreadyRanRows(CATALOG, NO_FILTERS)).toBe(0);
  });
});

describe('buildProjectGroups', () => {
  const GROUP_FILTERS = { providerFilter: 'all', query: '', showAlreadyRan: false } as const;

  it('lists every project regardless of the provider filter', () => {
    const groups = buildProjectGroups(CATALOG, { ...GROUP_FILTERS, providerFilter: 'codex' });
    expect(groups.map((g) => g.key)).toEqual(['p-alpha', 'p-beta']);
    expect(groups.map((g) => g.label)).toEqual(['alpha', 'beta']);
    expect(groups.every((g) => g.known)).toBe(true);
  });

  it('merges every cwd of one project into a single entry', () => {
    // The bug this exists for: /repo and /repo/frontend listed twice under
    // the same name, with the rows split between them.
    const catalog = [
      row('sub', { projectPath: '/repos/alpha/frontend' }),
      row('root', { projectPath: '/repos/alpha' }),
      row('deep', { projectPath: '/repos/alpha/internal/store' }),
    ];
    const groups = buildProjectGroups(catalog, GROUP_FILTERS);
    expect(groups).toEqual([
      // The representative path is the SHORTEST member cwd — the project's
      // own root is not on any row, and this is the closest stand-in.
      { key: 'p-alpha', path: '/repos/alpha', label: 'alpha', count: 3, known: true },
    ]);
  });

  it('tiebreaks the path only — the label is a property of the group', () => {
    // Every row of a known project carries that project's own name (the scan
    // stamps `ProjectLabel = project.Name`) and an unknown group is keyed on
    // the path itself, so the path is the only field members can disagree on.
    const groups = buildProjectGroups(
      [
        row('sub', { projectPath: '/repos/alpha/frontend' }),
        row('root', { projectPath: '/repos/alpha' }),
      ],
      GROUP_FILTERS,
    );
    expect(groups.map((g) => [g.path, g.label])).toEqual([['/repos/alpha', 'alpha']]);
  });

  it('keeps project-less rows keyed by path, one entry per cwd', () => {
    const groups = buildProjectGroups(
      [unknownProjectRow('a', '/tmp/one'), unknownProjectRow('b', '/tmp/two')],
      GROUP_FILTERS,
    );
    expect(groups.map((g) => [g.key, g.label, g.known])).toEqual([
      ['/tmp/one', 'one', false],
      ['/tmp/two', 'two', false],
    ]);
  });

  it('counts only rows surviving the provider filter', () => {
    const all = buildProjectGroups(CATALOG, GROUP_FILTERS);
    expect(all.map((g) => g.count)).toEqual([2, 2]);

    const claude = buildProjectGroups(CATALOG, { ...GROUP_FILTERS, providerFilter: 'claude' });
    expect(claude.map((g) => [g.key, g.count])).toEqual([
      ['p-alpha', 1],
      ['p-beta', 1],
    ]);
  });

  it('keeps a group whose count the filters drove to zero', () => {
    const groups = buildProjectGroups(CATALOG, {
      ...GROUP_FILTERS,
      providerFilter: 'claude',
      query: 'router',
    });
    expect(groups.map((g) => [g.key, g.count])).toEqual([
      ['p-alpha', 0],
      ['p-beta', 0],
    ]);
  });

  it('ignores the project filter entirely — the menu must not shrink to its own pick', () => {
    const groups = buildProjectGroups(CATALOG, { ...GROUP_FILTERS, query: 'sweep' });
    expect(groups.map((g) => [g.key, g.count])).toEqual([
      ['p-beta', 1],
      ['p-alpha', 0],
    ]);
  });

  it('orders by count descending, so the projects a home is made of come first', () => {
    const catalog = [
      row('a1', { projectPath: '/repos/a', projectId: 'p-a', projectLabel: 'a' }),
      row('b1', { projectPath: '/repos/b', projectId: 'p-b', projectLabel: 'b' }),
      row('b2', { projectPath: '/repos/b', projectId: 'p-b', projectLabel: 'b' }),
      row('c1', { projectPath: '/repos/c', projectId: 'p-c', projectLabel: 'c' }),
      row('b3', { projectPath: '/repos/b/sub', projectId: 'p-b', projectLabel: 'b' }),
    ];
    expect(buildProjectGroups(catalog, GROUP_FILTERS).map((g) => [g.label, g.count])).toEqual([
      ['b', 3],
      ['a', 1],
      ['c', 1],
    ]);
  });

  it('orders count ties by label then path, so scan order cannot reshuffle the menu', () => {
    const dupes = [
      unknownProjectRow('x', '/repos/z', { projectLabel: 'same' }),
      unknownProjectRow('y', '/repos/a', { projectLabel: 'Same' }),
    ];
    expect(buildProjectGroups(dupes, GROUP_FILTERS).map((g) => g.key)).toEqual([
      '/repos/a',
      '/repos/z',
    ]);
  });

  it('drops a project whose every row is withheld, and its counts elsewhere', () => {
    const catalog = [
      row('mine'),
      row('ao', {
        ranInAgentOverflow: true,
        projectPath: '/repos/beta',
        projectId: 'p-beta',
        projectLabel: 'beta',
      }),
    ];
    // A project with nothing on offer is not a choice; showing it as "beta
    // (0)" would be a menu entry that can only ever produce an empty list.
    expect(buildProjectGroups(catalog, GROUP_FILTERS).map((g) => g.key)).toEqual(['p-alpha']);
    expect(
      buildProjectGroups(catalog, { ...GROUP_FILTERS, showAlreadyRan: true }).map((g) => [
        g.key,
        g.count,
      ]),
    ).toEqual([
      ['p-alpha', 1],
      ['p-beta', 1],
    ]);
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

  // One session maps to one thread, so a thread figure would only repeat the
  // selection count and does not belong in the footer.
  it('does not repeat the one-thread-per-session count', () => {
    const catalog = [row('a'), row('b')];
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
      hiddenCount: 0,
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

  it('separates an empty view the toggle is causing from one the filters are', () => {
    // Rows exist and are importable, so neither "nothing to import" nor "no
    // matches" is true — and Clear filters would not bring them back.
    expect(surface({ rowCount: 5, filteredCount: 0, hiddenCount: 5 })).toBe('hidden-only');
    expect(surface({ rowCount: 5, filteredCount: 0, hiddenCount: 0 })).toBe('no-matches');
    // A catalog of nothing but already-ran rows is the same state, not the
    // "everything is already here" one — those rows can still be imported.
    expect(surface({ rowCount: 3, filteredCount: 0, hiddenCount: 3 })).toBe('hidden-only');
  });

  it('never reports hidden-only over a view that has rows', () => {
    expect(surface({ rowCount: 5, filteredCount: 2, hiddenCount: 3 })).toBe('rows');
  });

  it('keeps the toolbar for every catalog-shaped state, so each empty one is escapable', () => {
    expect(surfaceHasCatalog('rows')).toBe(true);
    expect(surfaceHasCatalog('empty')).toBe(true);
    expect(surfaceHasCatalog('no-matches')).toBe(true);
    expect(surfaceHasCatalog('hidden-only')).toBe(true);
    expect(surfaceHasCatalog('loading')).toBe(false);
    expect(surfaceHasCatalog('error')).toBe(false);
    expect(surfaceHasCatalog('unavailable')).toBe(false);
  });
});
