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
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';
import { BUNDLE_ID_FILE, BUNDLE_RELEASE_FILE, RELEASE_BUILD_ENV, bundleFiles, bundleId, bundleIdPlugin, bundleReleaseVersion, computeBundleId, included, nextPatchVersion, stampBundle } from '../../scripts/bundleId';
import { compareBundleVersions } from '../lib/native/bundleVersion';

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

describe('the stamped release version', () => {
  it('is the package version for a release build', () => {
    expect(bundleReleaseVersion('1.2.3', true)).toBe('1.2.3');
    expect(bundleReleaseVersion('1.2.3-rc.1+build.4', true)).toBe('1.2.3-rc.1+build.4');
  });

  it('is a development prerelease of the next patch for every other build', () => {
    expect(bundleReleaseVersion('1.2.3', false, 1_754_000_000_500)).toBe('1.2.4-dev.1754000000');
    expect(bundleReleaseVersion('1.2.3-rc.1+build.4', false, 1_754_000_000_500)).toBe('1.2.4-dev.1754000000');
    expect(nextPatchVersion('1.2.9')).toBe('1.2.10');
  });

  it('orders development builds forward, below the release they precede, above the release they follow', () => {
    const earlier = bundleReleaseVersion('1.2.3', false, 1_754_000_000_000);
    const later = bundleReleaseVersion('1.2.3', false, 1_754_000_001_000);
    expect(compareBundleVersions(later, earlier)).toBe(1);
    expect(compareBundleVersions(earlier, '1.2.3')).toBe(1);
    expect(compareBundleVersions('1.2.4', later)).toBe(1);
    expect(compareBundleVersions('1.2.4-rc.1', later)).toBe(1);
  });

  it.each([undefined, null, 12, '', 'dev', 'v1.2.3', '1.2', '01.2.3', '1.2.3-01', '1.2.3\n', '1.2.3-', '1.2.3+'])('rejects invalid package version %j', (version) => {
    expect(() => bundleReleaseVersion(version, false)).toThrow('semantic version');
    expect(() => bundleReleaseVersion(version, true)).toThrow('semantic version');
  });
});

describe('release identity in built bundles', () => {
  const releaseBuild = process.env[RELEASE_BUILD_ENV];
  afterEach(() => {
    if (releaseBuild === undefined) delete process.env[RELEASE_BUILD_ENV];
    else process.env[RELEASE_BUILD_ENV] = releaseBuild;
  });

  async function buildFixture(run: (root: string, output: string) => Promise<void>): Promise<void> {
    const root = await mkdtemp(resolve(tmpdir(), 'ao-bundle-release-'));
    try {
      const output = resolve(root, 'dist');
      await mkdir(output);
      await writeFile(resolve(output, 'index.html'), '<html>UI</html>');
      await writeFile(resolve(root, 'package.json'), JSON.stringify({ version: '1.2.3' }));
      await run(root, output);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }

  it('stamps the package version in a release build before hashing all release bytes', async () => {
    process.env[RELEASE_BUILD_ENV] = '1';
    await buildFixture(async (root, output) => {
      const plugin = bundleIdPlugin();
      plugin.configResolved({ root, build: { outDir: 'dist' } });
      await plugin.closeBundle();
      expect(JSON.parse(await readFile(resolve(output, BUNDLE_RELEASE_FILE), 'utf8'))).toEqual({ version: '1.2.3' });
      const first = (await readFile(resolve(output, BUNDLE_ID_FILE), 'utf8')).trim();
      expect(first).toBe(await computeBundleId(output));
      expect((await bundleFiles(output)).map((file) => file.path)).toContain(BUNDLE_RELEASE_FILE);
      await stampBundle(output, '1.2.4');
      expect(await computeBundleId(output)).not.toBe(first);
    });
  });

  it('stamps a development prerelease of the next patch in any other build', async () => {
    delete process.env[RELEASE_BUILD_ENV];
    await buildFixture(async (root, output) => {
      const before = Math.floor(Date.now() / 1000);
      const plugin = bundleIdPlugin();
      plugin.configResolved({ root, build: { outDir: 'dist' } });
      await plugin.closeBundle();
      const { version } = JSON.parse(await readFile(resolve(output, BUNDLE_RELEASE_FILE), 'utf8')) as { version: string };
      const match = /^1\.2\.4-dev\.(\d+)$/.exec(version);
      expect(match).not.toBeNull();
      expect(Number(match![1])).toBeGreaterThanOrEqual(before);
      expect(compareBundleVersions(version, '1.2.3')).toBe(1);
      expect(compareBundleVersions('1.2.4', version)).toBe(1);
    });
  });

  it.each([undefined, null, 12, '', 'dev', 'v1.2.3', '1.2', '01.2.3', '1.2.3-01', '1.2.3\n', '1.2.3-', '1.2.3+'])('rejects invalid package version %j before writing', async (version) => {
    await expect(stampBundle('/not-a-real-build-directory', version)).rejects.toThrow('semantic version');
  });
});
