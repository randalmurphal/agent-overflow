import { describe, expect, it } from 'vitest';
import type { PatchFile } from './patchFiles';
import {
  buildReviewTree,
  comparePathsTreeOrder,
  fileExtensionLabel,
  filterReviewFiles,
  flattenReviewTree,
  isTestPath,
  sortFilesTreeOrder,
} from './reviewTree';

function file(path: string, additions = 0, deletions = 0, kind = 'modified'): PatchFile {
  return { path, kind, additions, deletions, lines: [] };
}

describe('reviewTree', () => {
  it('builds nested directory nodes with file leaves', () => {
    const tree = buildReviewTree([
      file('src/app.ts', 3, 1),
      file('test/app.test.ts', 2, 0),
    ]);

    expect(tree).toMatchObject([
      {
        kind: 'directory',
        name: 'src',
        path: 'src',
        children: [
          { kind: 'file', name: 'app.ts', path: 'src/app.ts', fileIndex: 0 },
        ],
      },
      {
        kind: 'directory',
        name: 'test',
        path: 'test',
        children: [
          { kind: 'file', name: 'app.test.ts', path: 'test/app.test.ts', fileIndex: 1 },
        ],
      },
    ]);
  });

  it('compresses single-child directory chains like GitHub', () => {
    const tree = buildReviewTree([
      file('src/lib/components/Button.svelte'),
      file('src/lib/utils/path.ts'),
    ]);

    expect(tree).toMatchObject([
      {
        kind: 'directory',
        name: 'src/lib',
        path: 'src/lib',
        children: [
          { kind: 'directory', name: 'components', path: 'src/lib/components' },
          { kind: 'directory', name: 'utils', path: 'src/lib/utils' },
        ],
      },
    ]);
  });

  it('keeps file badge counts and flattening respects collapsed dirs', () => {
    const tree = buildReviewTree([
      file('src/lib/app.ts', 7, 2),
      file('README.md', 1, 0),
    ]);
    const src = tree.find((node) => node.kind === 'directory');
    expect(src).toMatchObject({ kind: 'directory', path: 'src/lib' });

    const all = flattenReviewTree(tree, new Set());
    expect(all.map((entry) => [entry.depth, entry.node.kind, entry.node.path])).toEqual([
      [0, 'directory', 'src/lib'],
      [1, 'file', 'src/lib/app.ts'],
      [0, 'file', 'README.md'],
    ]);
    expect(all[1]?.node).toMatchObject({
      kind: 'file',
      additions: 7,
      deletions: 2,
    });

    const collapsed = flattenReviewTree(tree, new Set(['src/lib']));
    expect(collapsed.map((entry) => entry.node.path)).toEqual(['src/lib', 'README.md']);
  });

  it('carries the patch kind onto file nodes and honors explicit file indexes', () => {
    // A filtered list: positions 0/1 here are indexes 2/5 in the full diff.
    const tree = buildReviewTree(
      [file('src/added.ts', 4, 0, 'added'), file('src/gone.ts', 0, 9, 'deleted')],
      [2, 5],
    );

    expect(tree).toMatchObject([
      {
        kind: 'directory',
        path: 'src',
        children: [
          { kind: 'file', path: 'src/added.ts', fileIndex: 2, fileKind: 'added' },
          { kind: 'file', path: 'src/gone.ts', fileIndex: 5, fileKind: 'deleted' },
        ],
      },
    ]);
  });
});

describe('sortFilesTreeOrder', () => {
  it('sorts dirs-first at every level, root files last', () => {
    // Git patch order is plain lexicographic: root files interleave
    // between directories (src/ < stack.config < tests/).
    const gitOrder = [
      file('docs/data-topology.md'),
      file('infra/config.yaml'),
      file('src/core/config.py'),
      file('src/runtime.py'),
      file('stack.config.example.json'),
      file('tests/unit/test_runtime.py'),
    ];
    const sorted = sortFilesTreeOrder(gitOrder);
    expect(sorted.map((f) => f.path)).toEqual([
      'docs/data-topology.md',
      'infra/config.yaml',
      'src/core/config.py',
      'src/runtime.py',
      'tests/unit/test_runtime.py',
      'stack.config.example.json',
    ]);
    // Input untouched — parse results are shared immutable arrays.
    expect(gitOrder[4].path).toBe('stack.config.example.json');
  });

  it('matches the tree render order exactly', () => {
    const files = sortFilesTreeOrder([
      file('b.md'),
      file('src/z/deep.ts'),
      file('src/a.ts'),
      file('a/nested.ts'),
      file('src/z/aaa.ts'),
    ]);
    const rendered = flattenReviewTree(buildReviewTree(files), new Set())
      .filter((entry) => entry.node.kind === 'file')
      .map((entry) => entry.node.path);
    expect(files.map((f) => f.path)).toEqual(rendered);
  });

  it('orders a directory before a same-named file', () => {
    expect(comparePathsTreeOrder('a/b/c.ts', 'a/b')).toBeLessThan(0);
    expect(comparePathsTreeOrder('a/b', 'a/b/c.ts')).toBeGreaterThan(0);
    expect(comparePathsTreeOrder('a/b', 'a/b')).toBe(0);
  });
});

describe('fileExtensionLabel', () => {
  it('reports the extension, or the whole name when the name is the type', () => {
    expect(fileExtensionLabel('src/lib/app.ts')).toBe('.ts');
    expect(fileExtensionLabel('a/b/Component.test.svelte')).toBe('.svelte');
    expect(fileExtensionLabel('Makefile')).toBe('Makefile');
    expect(fileExtensionLabel('src/.gitignore')).toBe('.gitignore');
  });
});

describe('filterReviewFiles', () => {
  const files = [
    file('src/app.ts'),
    file('src/App.svelte'),
    file('docs/readme.md'),
    file('Makefile'),
  ];

  it('matches the path substring case-insensitively and keeps original indexes', () => {
    const result = filterReviewFiles(files, 'APP', new Set());
    expect(result.files.map((f) => f.path)).toEqual(['src/app.ts', 'src/App.svelte']);
    expect(result.fileIndexes).toEqual([0, 1]);

    const docs = filterReviewFiles(files, 'docs/', new Set());
    expect(docs.fileIndexes).toEqual([2]);
  });

  it('intersects the extension filter with the query', () => {
    const svelteOnly = filterReviewFiles(files, '', new Set(['.svelte']));
    expect(svelteOnly.files.map((f) => f.path)).toEqual(['src/App.svelte']);

    const both = filterReviewFiles(files, 'app', new Set(['.ts', '.md']));
    expect(both.files.map((f) => f.path)).toEqual(['src/app.ts']);

    const nameAsType = filterReviewFiles(files, '', new Set(['Makefile']));
    expect(nameAsType.fileIndexes).toEqual([3]);
  });

  it('passes everything through when no filter is active', () => {
    const result = filterReviewFiles(files, '  ', new Set());
    expect(result.files).toHaveLength(4);
    expect(result.fileIndexes).toEqual([0, 1, 2, 3]);
  });
  it('drops test files when hideTests is set, composed with the extension set', () => {
    const mixed = [
      file('internal/git/gitlab.go'),
      file('internal/git/gitlab_test.go'),
      file('src/app.ts'),
      file('src/app.test.ts'),
      file('e2e/tests/review-overview.spec.ts'),
      file('docs/readme.md'),
    ];
    const shown = filterReviewFiles(mixed, '', new Set(['.ts', '.go']), true);
    expect(shown.files.map((f) => f.path)).toEqual(['internal/git/gitlab.go', 'src/app.ts']);
    expect(shown.fileIndexes).toEqual([0, 2]);

    const all = filterReviewFiles(mixed, '', new Set(['.ts', '.go']));
    expect(all.fileIndexes).toEqual([0, 1, 2, 3, 4]);
  });
});

describe('isTestPath', () => {
  it.each<[string, boolean]>([
    // TS/JS/Svelte: .test or .spec right before the extension.
    ['src/lib/utils/reviewTree.test.ts', true],
    ['src/Panel.SPEC.tsx', true],
    ['src/lib/Pane.test.svelte', true],
    ['scripts/build.test.mjs', true],
    ['e2e/tests/review-overview.spec.ts', true],
    ['src/foo.testing.ts', false],
    ['src/test.helpers.ts', false],
    ['e2e/tests/forge-helpers.ts', false],
    ['src/testing/helpers.ts', false],
    ['src/latest.ts', false],
    // TS/JS: __tests__ and __mocks__ directory segments, matched exactly.
    ['src/__tests__/helpers.js', true],
    ['src/__mocks__/fs.cjs', true],
    ['src/__Tests__/helpers.js', false],
    ['src/__tests__.ts', false],
    // Go.
    ['internal/git/gitlab_test.go', true],
    ['internal/git/gitlab.go', false],
    ['internal/git/latest.go', false],
    // Python.
    ['pkg/test_models.py', true],
    ['pkg/models_test.py', true],
    ['pkg/conftest.py', true],
    ['tests/helpers.py', true],
    ['pkg/test/fixtures.py', true],
    ['src/contest.py', false],
    ['src/testing/helpers.py', false],
    ['pkg/models.py', false],
    // Java: CamelCase suffixes and a src/test segment pair anywhere.
    ['src/test/java/FooTest.java', true],
    ['core/src/test/java/Helpers.java', true],
    ['src/main/java/FooTest.java', true],
    ['src/main/java/FooTests.java', true],
    ['src/main/java/FooIT.java', true],
    ['src/main/java/Latest.java', false],
    ['src/main/java/Edit.java', false],
    ['src/main/java/Foo.java', false],
    ['test/src/main/Foo.java', false],
    // C/C++.
    ['lib/parser_test.c', true],
    ['lib/test_parser.cpp', true],
    ['lib/parser_TEST.cc', true],
    ['tests/fixtures.cxx', true],
    ['test/helpers.c', true],
    ['lib/latest.cpp', false],
    ['testsuite/helpers.c', false],
    ['tests/fixtures.h', false],
    // Rust.
    ['tests/integration.rs', true],
    ['src/parser_test.rs', true],
    ['src/tests.rs', true],
    ['test/integration.rs', false],
    ['src/contests.rs', false],
    // Bash and PowerShell.
    ['test/cli.bats', true],
    ['scripts/install.sh', false],
    ['tests/install.sh', false],
    ['scripts/Install.Tests.ps1', true],
    ['scripts/Install.ps1', false],
    ['scripts/InstallTests.ps1', false],
    // Everything else.
    ['latest_test_results.md', false],
    ['tests/fixtures.json', false],
    ['Makefile', false],
    ['src/.test', false],
  ])('%s -> %s', (path, expected) => {
    expect(isTestPath(path)).toBe(expected);
  });
});
