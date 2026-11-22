// Decides which CI suites a pull request runs, from the paths it changes.
// These rules live only here; docs/architecture/development.md points at
// this file.
//
//   node scripts/ci-suites.mjs PATHS_FILE   a pull request's changed paths
//   node scripts/ci-suites.mjs --all        a push or a manual run
//
// It prints GITHUB_OUTPUT lines, each true or false: go-checks, go-lint,
// frontend, vitest, e2e and upgrade-lineage. The Go test shards always run:
// the Go test cache decides which packages a change reaches, through the
// files each test reads. A change to the Playwright suite runs all of it:
// Playwright's --only-changed misses a spec that reaches a changed helper
// through another helper (review-diff-reading.spec.ts through
// review-diff-reading-flow.ts to review-comment-actions-flow.ts).
//
// Every path falls in the first class that matches it, and a suite runs
// when the change holds a path of a class it reads. A path no class
// matches runs every suite. Each class lists only what a suite provably
// does not read, so a new kind of file costs time, never coverage.
import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');

// harnessTestPackages are the packages `make harness-build` compiles into
// test binaries for the Playwright suite (HARNESS_TEST_PKGS), whose test
// files are therefore Playwright inputs.
export function harnessTestPackages(makefile = readFileSync(join(root, 'Makefile'), 'utf8')) {
  const line = /^HARNESS_TEST_PKGS :=(.*)$/m.exec(makefile);
  if (!line) throw new Error('the Makefile no longer sets HARNESS_TEST_PKGS');
  const packages = line[1].trim().split(/\s+/).filter(Boolean).map(pkg => pkg.replace(/^\.\//, ''));
  if (packages.length === 0 || packages.some(pkg => !/^[\w./-]+$/.test(pkg) || pkg.includes('...'))) {
    throw new Error(`HARNESS_TEST_PKGS names no plain package directories: ${line[1].trim()}`);
  }
  return packages;
}

// classify names a path's class. Searched before each class was added:
// no Go package embeds, and no Vitest, Playwright or lint source reads, a
// file under docs/ or a guide file; Vitest and Playwright read no _test.go
// file; svelte-check and the bundle read nothing outside frontend/ but the
// testdata the frontend tests import, which falls through to "other".
export function classify(path, harnessPackages) {
  const base = path.slice(path.lastIndexOf('/') + 1);
  if (path.startsWith('docs/') || base === 'AGENTS.md' || base === 'CLAUDE.md' || path === 'README.md') return 'docs';
  if (path.startsWith('e2e/')) return 'e2e';
  if (path.startsWith('frontend/')) {
    if (/^frontend\/src\/(test\/|.+\.(test|spec)\.[tj]s$|.+\.manual\.ts$)/.test(path)) return 'frontend-test';
    return 'frontend';
  }
  if (path.endsWith('_test.go')) {
    return harnessPackages.includes(dirname(path)) ? 'go' : 'go-test';
  }
  if (path.endsWith('.go') || path === 'go.mod' || path === 'go.sum') return 'go';
  return 'other';
}

// reads lists the classes each suite reads.
const reads = {
  'go-checks': ['go', 'go-test', 'frontend'],
  'go-lint': ['go', 'go-test'],
  frontend: ['frontend', 'frontend-test'],
  vitest: ['frontend', 'frontend-test', 'go'],
  e2e: ['frontend', 'go', 'e2e'],
};

// upgradeLineage matches what the upgrade lineage check can break on: the
// store, the check and its corpus, and the version bump.
const upgradeLineage = /^(internal\/store\/|e2e\/tests\/upgrade-corpus|scripts\/release-upgrade-check\.sh$|build\/config\.yml$|frontend\/package\.json$)/;

export function suites(paths, harnessPackages) {
  const classes = new Set(paths.map(path => classify(path, harnessPackages)));
  const runs = suite => classes.has('other') || reads[suite].some(name => classes.has(name));
  return {
    'go-checks': runs('go-checks'),
    'go-lint': runs('go-lint'),
    frontend: runs('frontend'),
    vitest: runs('vitest'),
    e2e: runs('e2e'),
    'upgrade-lineage': paths.some(path => upgradeLineage.test(path)),
  };
}

export const everything = { 'go-checks': true, 'go-lint': true, frontend: true, vitest: true, e2e: true, 'upgrade-lineage': true };

function main(args) {
  let result;
  if (args.length === 1 && args[0] === '--all') result = everything;
  else if (args.length === 1) {
    const paths = readFileSync(args[0], 'utf8').split('\n').filter(Boolean);
    result = suites(paths, harnessTestPackages());
  } else throw new Error('usage: ci-suites.mjs PATHS_FILE | --all');
  for (const [name, value] of Object.entries(result)) console.log(`${name}=${value}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    console.error(`ci-suites: ${error.message}`);
    process.exitCode = 1;
  }
}
