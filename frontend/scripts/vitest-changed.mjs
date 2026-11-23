#!/usr/bin/env node
// Runs a Vitest command on what a diff can affect: with `--changed <ref>`
// when that selection is complete, and on the projects whole when it is not.
//
//   cd frontend
//   ../bin/ao-netns node scripts/vitest-changed.mjs origin/main run --project unit --project unit-isolated
//
// The diff is what `--changed <ref>` reads: the commits since the merge
// base with <ref>, the staged changes and the working tree.
//
// `--changed` selects the test files whose imports reach a changed file. A
// deleted file, and the old path of a renamed one, is in no import graph
// any more, so the tests that read it are not selected though their result
// can change: a literal import of it now fails, and a glob that matched it
// matches less. So when the diff deletes or renames a path that a test can
// read, the projects run whole, and this says which path and why. A test
// can read any file under frontend/, and a file outside it only through an
// import of a path outside it (a `?raw` read of a Go source, a glob over
// harness scenarios), which this lists from the sources under src/.
import { execFileSync, spawnSync } from 'node:child_process';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, isAbsolute, join, matchesGlob, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const FRONTEND = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SOURCE_FILE = /\.(?:[cm]?[jt]s|svelte)$/;
const IMPORT = /(?:\bfrom|\bimport)\s*\(?\s*(['"])([^'"\n]+)\1/g;
const GLOB = /import\.meta\.glob(?:<[^>]*>)?\(\s*(\[[^\]]*\]|'[^']*'|"[^"]*")/g;
const QUOTED = /(['"])([^'"]+)\1/g;

function posix(path) {
  return path.split(sep).join('/');
}

/** What a test can read in `repo`: the frontend directory, and the paths
 * outside it that a source under src/ imports, as repository-relative
 * literal paths and glob patterns. */
export function testReads(repo, frontend = FRONTEND) {
  const literals = new Set();
  const globs = new Set();
  const outside = (file, specifier) => {
    const path = specifier.split('?')[0];
    if (!path.startsWith('.')) return null;
    const target = resolve(dirname(file), path);
    const fromFrontend = relative(frontend, target);
    if (!fromFrontend.startsWith('..') && !isAbsolute(fromFrontend)) return null;
    return posix(relative(repo, target));
  };
  for (const entry of readdirSync(join(frontend, 'src'), { recursive: true, withFileTypes: true })) {
    if (!entry.isFile() || !SOURCE_FILE.test(entry.name)) continue;
    const file = join(entry.parentPath, entry.name);
    const text = readFileSync(file, 'utf8');
    for (const [, , specifier] of text.matchAll(IMPORT)) {
      const path = outside(file, specifier);
      if (path !== null) literals.add(path);
    }
    for (const [, patterns] of text.matchAll(GLOB)) {
      for (const [, , pattern] of patterns.matchAll(QUOTED)) {
        const path = outside(file, pattern.replace(/^!/, ''));
        if (path !== null && !pattern.startsWith('!')) globs.add(path);
      }
    }
  }
  return { frontendDir: posix(relative(repo, frontend)), literals, globs };
}

/** Why a test can read the repository-relative `path`, or null. */
export function readBy(path, reads) {
  if (path.startsWith(`${reads.frontendDir}/`)) return `it is under ${reads.frontendDir}/`;
  if (reads.literals.has(path)) return 'a source under src/ imports it';
  for (const glob of reads.globs) {
    if (matchesGlob(path, glob)) return `a source under src/ imports the glob ${glob}`;
  }
  return null;
}

/** The deleted paths and renamed-from paths in `git diff --name-status -z`
 * output. */
export function removedPaths(nameStatus) {
  const fields = nameStatus.split('\0').filter((field) => field !== '');
  const removed = [];
  for (let index = 0; index < fields.length;) {
    const status = fields[index];
    if (status.startsWith('R') || status.startsWith('C')) {
      if (status.startsWith('R')) removed.push(fields[index + 1]);
      index += 3;
    } else {
      if (status === 'D') removed.push(fields[index + 1]);
      index += 2;
    }
  }
  return removed;
}

/** The first removed path a test can read, with why, or null. */
export function wholeRunReason(removed, reads) {
  for (const path of removed) {
    const why = readBy(path, reads);
    if (why !== null) return `${path} is deleted or renamed in the diff, and a test can read it (${why})`;
  }
  return null;
}

function git(repo, args) {
  return execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
}

function main(argv) {
  const [ref, ...vitestArgs] = argv;
  if (ref === undefined || ref.startsWith('-') || vitestArgs.length === 0) {
    throw new Error('usage: node scripts/vitest-changed.mjs <ref> <vitest command and arguments>');
  }
  const repo = git(FRONTEND, ['rev-parse', '--show-toplevel']).trim();
  const removed = [
    ...removedPaths(git(repo, ['diff', '--name-status', '-z', '-M', `${ref}...HEAD`])),
    ...removedPaths(git(repo, ['diff', '--cached', '--name-status', '-z', '-M'])),
    ...removedPaths(git(repo, ['diff', '--name-status', '-z', '-M'])),
  ];
  const reason = removed.length === 0 ? null : wholeRunReason(removed, testReads(repo));
  if (reason !== null) console.log(`vitest-changed: running the projects whole: ${reason}.`);
  const args = reason === null ? [...vitestArgs, '--changed', ref] : vitestArgs;
  const result = spawnSync(join(FRONTEND, 'node_modules/.bin/vitest'), args, { cwd: FRONTEND, stdio: 'inherit' });
  if (result.error !== undefined) throw result.error;
  process.exit(result.status ?? 1);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main(process.argv.slice(2));
