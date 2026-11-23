#!/usr/bin/env node
// Runs the unit or browser tests a diff can affect: with `--changed <ref>`
// when that selection is complete, and the projects whole when it is not.
//
//   cd frontend
//   ../bin/ao-netns pnpm run test:affected origin/main
//   ../bin/ao-netns pnpm run test:browser:affected origin/main
//
// which run `node scripts/vitest-changed.mjs <unit|browser> <ref> [vitest
// arguments]`. `--print` prints the Vitest command instead of running it.
//
// The diff is what `--changed <ref>` reads: the commits since the merge
// base with <ref>, the staged changes and the working tree.
//
// `--changed` selects the test files whose imports reach a changed file. A
// deleted file, and the old path of a renamed one, is in no import graph
// any more, so the tests that read it are not selected though their result
// can change: a literal import of it now fails, and a glob that matched it
// matches less. Each such path is decided from the path first:
//   - a test file, or a file outside frontend/, runs the projects whole only
//     when a source under src/ imports it or a glob there matches it;
//   - any other file under frontend/ runs the projects whole, since config,
//     styles and HTML read it without an import.
// Only the sources that name the path's file name or call import.meta.glob
// are read to decide.
import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { basename, dirname, join, matchesGlob, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const FRONTEND = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const MODES = {
  unit: ['run', '--project', 'unit', '--project', 'unit-isolated'],
  browser: ['run', '--project', 'browser'],
};
const TEST_FILE = /\.(?:test|spec)\.[tj]s$|\.manual\.ts$/;
const IMPORT = /(?:\bfrom|\bimport)\s*\(?\s*(['"])([^'"\n]+)\1/g;
const GLOB = /import\.meta\.glob(?:<[^>]*>)?\(\s*(\[[^\]]*\]|'[^']*'|"[^"]*")/g;
const QUOTED = /(['"])([^'"]+)\1/g;

function posix(path) {
  return path.split(sep).join('/');
}

function git(repo, args) {
  return execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
}

/** What the sources `files` (absolute) read, as repository-relative literal
 * paths and glob patterns. */
export function sourceReads(repo, frontend, files) {
  const literals = new Set();
  const globs = new Set();
  const target = (file, specifier) => {
    const path = specifier.split('?')[0];
    if (path.startsWith('.')) return posix(relative(repo, resolve(dirname(file), path)));
    // Vite resolves a leading slash from the project root.
    if (path.startsWith('/')) return posix(relative(repo, join(frontend, path)));
    return null;
  };
  for (const file of files) {
    const text = readFileSync(file, 'utf8');
    for (const [, , specifier] of text.matchAll(IMPORT)) {
      const path = target(file, specifier);
      if (path !== null) literals.add(path);
    }
    for (const [, patterns] of text.matchAll(GLOB)) {
      for (const [, , pattern] of patterns.matchAll(QUOTED)) {
        if (pattern.startsWith('!')) continue;
        const path = target(file, pattern);
        if (path !== null) globs.add(path);
      }
    }
  }
  return { literals, globs };
}

/** Why a source reads the repository-relative `path`, or null. */
export function readBy(path, reads) {
  // An import names a module without its extension, or a directory for its
  // index.
  const bare = path.replace(/(?:\.svelte)?\.[cm]?[jt]s$/, '');
  const spellings = [path, bare, ...(bare.endsWith('/index') ? [bare.slice(0, -'/index'.length)] : [])];
  if (spellings.some((spelling) => reads.literals.has(spelling))) return 'a source under src/ imports it';
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

/** The sources under src/ that could read one of `paths`: those naming its
 * file name, and those calling import.meta.glob. */
function candidateSources(repo, frontend, paths) {
  const src = posix(relative(repo, join(frontend, 'src')));
  // The file name as an import spells it, after a slash: without a module's
  // extension, and ended by a quote, a query or an extension.
  const names = paths.map((path) => basename(path).replace(/(?:\.svelte)?\.[cm]?[jt]s$/, ''));
  const patterns = [...new Set(names)].flatMap((name) => ['-e', `/${name.replace(/[.[\]()*+?^$|\\{}]/g, '\\$&')}([.?'"]|$)`]);
  let listed;
  try {
    listed = git(repo, ['grep', '-l', '--untracked', '-E', ...patterns, '-e', 'import\\.meta\\.glob', '--', src]);
  } catch (error) {
    // git grep exits 1 when nothing matches.
    if (error.status === 1) return [];
    throw error;
  }
  return listed.split('\n').filter((line) => line !== '').map((line) => join(repo, line));
}

/** Why the removed `paths` need the projects run whole, or null. */
export function wholeRunReason(repo, frontend, paths) {
  const frontendDir = posix(relative(repo, frontend));
  const decidedByReads = [];
  for (const path of paths) {
    const inFrontend = path.startsWith(`${frontendDir}/`);
    if (inFrontend && !TEST_FILE.test(path)) {
      return `${path} is deleted or renamed in the diff, and a test can read it (it is under ${frontendDir}/)`;
    }
    decidedByReads.push(path);
  }
  if (decidedByReads.length === 0) return null;
  const reads = sourceReads(repo, frontend, candidateSources(repo, frontend, decidedByReads));
  for (const path of decidedByReads) {
    const why = readBy(path, reads);
    if (why !== null) return `${path} is deleted or renamed in the diff, and a test can read it (${why})`;
  }
  return null;
}

/** The Vitest arguments for `mode` against `ref` in `repo`, and why the
 * projects run whole when they do. */
export function affectedRun(repo, frontend, mode, ref, extra) {
  const removed = [
    ...removedPaths(git(repo, ['diff', '--name-status', '-z', '-M', `${ref}...HEAD`])),
    ...removedPaths(git(repo, ['diff', '--cached', '--name-status', '-z', '-M'])),
    ...removedPaths(git(repo, ['diff', '--name-status', '-z', '-M'])),
  ];
  const reason = removed.length === 0 ? null : wholeRunReason(repo, frontend, removed);
  const args = [...MODES[mode], ...extra, ...(reason === null ? ['--changed', ref] : [])];
  return { args, reason };
}

function main(argv) {
  const [mode, ref, ...rest] = argv;
  if (!Object.hasOwn(MODES, mode ?? '') || ref === undefined || ref.startsWith('-')) {
    throw new Error('usage: node scripts/vitest-changed.mjs <unit|browser> <ref> [--print] [vitest arguments]');
  }
  const print = rest.includes('--print');
  const extra = rest.filter((arg) => arg !== '--print');
  const repo = git(FRONTEND, ['rev-parse', '--show-toplevel']).trim();
  const { args, reason } = affectedRun(repo, FRONTEND, mode, ref, extra);
  if (reason !== null) console.log(`vitest-changed: running the projects whole: ${reason}.`);
  if (print) {
    console.log(`vitest ${args.join(' ')}`);
    return;
  }
  const result = spawnSync(join(FRONTEND, 'node_modules/.bin/vitest'), args, { cwd: FRONTEND, stdio: 'inherit' });
  if (result.error !== undefined) throw result.error;
  process.exit(result.status ?? 1);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main(process.argv.slice(2));
