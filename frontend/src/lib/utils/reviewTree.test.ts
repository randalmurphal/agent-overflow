import { describe, expect, it } from 'vitest';
import type { PatchFile } from './patchFiles';
import {
  buildReviewTree,
  comparePathsTreeOrder,
  fileExtensionLabel,
  filterReviewFiles,
  flattenReviewTree,
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
});
