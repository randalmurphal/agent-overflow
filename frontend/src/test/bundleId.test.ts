// The half of the bundle-id agreement that lives in this language.
//
// One rule, two implementations: `internal/bundle` (Go) hashes the
// embedded `frontend/dist` and publishes the answer on the hello frame,
// and `frontend/scripts/bundleId.ts` hashes the same tree at build time
// and writes `bundle-id.txt` into it. A phone compares the two, so a
// disagreement is a phone that downloads the bundle it is already
// running, on every connection, forever.
//
// They are pinned against each other by ONE fixture directory and ONE
// golden id, both under `internal/bundle/testdata/`. This file hashes
// the fixture with the TypeScript rule; `bundle_test.go` hashes it with
// the Go rule; both compare against `fixturebundle.id`. Changing the
// rule means changing both and re-stamping that file, which is what
// each side's failure message says.

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { bundleFiles, bundleId, computeBundleId, included } from '../../scripts/bundleId';

const TESTDATA = resolve(
  dirname(fileURLToPath(import.meta.url)),
  '..',
  '..',
  '..',
  'internal',
  'bundle',
  'testdata',
);
const FIXTURE = resolve(TESTDATA, 'fixturebundle');
const GOLDEN = resolve(TESTDATA, 'fixturebundle.id');

describe('the bundle id rule', () => {
  it('agrees with the Go implementation over the shared fixture', async () => {
    const golden = readFileSync(GOLDEN, 'utf8').trim();
    expect(
      await computeBundleId(FIXTURE),
      'Both this rule and internal/bundle hash the same fixture directory. '
        + 'If the rule changed on purpose, change BOTH and re-stamp '
        + 'internal/bundle/testdata/fixturebundle.id.',
    ).toBe(golden);
  });

  it('excludes source maps and the id file itself', async () => {
    // `bundle-id.txt` is written AFTER the walk, so a rule that counted
    // it would hash a tree the build never produced. Source maps are
    // emitted only under AO_SOURCEMAP=1, requested by no page, and
    // megabytes on a phone's link.
    expect(included('assets/index.js')).toBe(true);
    expect(included('bundle-id.txt')).toBe(false);
    expect(included('assets/index.js.map')).toBe(false);

    const files = await bundleFiles(FIXTURE);
    const paths = files.map((f) => f.path);
    expect(paths).toEqual(['README.txt', 'assets/index.js', 'index.html']);
  });

  it('sorts by path and reports each file exactly once', async () => {
    const files = await bundleFiles(FIXTURE);
    const sorted = [...files].map((f) => f.path).sort();
    expect(files.map((f) => f.path)).toEqual(sorted);
    expect(new Set(files.map((f) => f.path)).size).toBe(files.length);
    for (const file of files) {
      expect(file.sha256).toMatch(/^[0-9a-f]{64}$/);
      expect(file.size).toBeGreaterThan(0);
    }
  });

  it('is content-addressed, so order in does not change the answer', () => {
    const files = [
      { path: 'b.js', sha256: '1'.repeat(64), size: 1 },
      { path: 'a.js', sha256: '2'.repeat(64), size: 2 },
    ];
    expect(bundleId(files)).toBe(bundleId([...files].reverse()));
  });

  it('changes when a file changes its content, its name, or nothing at all', () => {
    const base = [{ path: 'a.js', sha256: '2'.repeat(64), size: 2 }];
    expect(bundleId(base)).toBe(bundleId([{ ...base[0] }]));
    expect(bundleId(base)).not.toBe(bundleId([{ ...base[0], sha256: '3'.repeat(64) }]));
    expect(bundleId(base)).not.toBe(bundleId([{ ...base[0], path: 'b.js' }]));
  });

  it('answers an empty tree with the hash of nothing rather than throwing', () => {
    // The Go side refuses an empty tree at the MANIFEST, where there is
    // a build to fail; the id rule itself is total, so the two never
    // disagree about what "nothing" hashes to.
    expect(bundleId([])).toMatch(/^[0-9a-f]{64}$/);
  });
});
