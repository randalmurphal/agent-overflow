#!/usr/bin/env node
// Checks that a change selects the tests that read it, the way
// `pnpm run test:affected <ref>` selects them:
//   - a change to a file selects its readers of each kind (a raw import of
//     a file that is not a module, a glob, a file outside frontend/, a
//     child-process workload), asked of Vitest's own related-files resolver;
//   - a change to an input every test reads runs every file only when it is
//     the frontend's own;
//   - a deletion or rename, made with git in a scratch repository, runs the
//     projects whole exactly when a test can read the removed path, through
//     the entry point the package script runs.
//
//   cd frontend
//   ../bin/ao-netns node scripts/vitest-changed.check.mjs
import { execFileSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { createVitest } from 'vitest/node';

const FRONTEND = resolve(import.meta.dirname, '..');

const READERS = [
  { kind: 'raw import of a file that is not a module', changed: 'index.html', reader: 'src/indexHtmlNoInlineScript.test.ts' },
  { kind: 'glob', changed: 'src/styles/syntax.css', reader: 'src/lib/styleInvalidation.test.ts' },
  { kind: 'file outside frontend/', changed: '../internal/stringsx/findfold_table.go', reader: 'src/lib/utils/findMatch.test.ts' },
  { kind: 'child-process workload', changed: 'src/lib/markdown/parser/index.ts', reader: 'src/lib/markdown/parseBlockRetention.test.ts' },
  { kind: 'fixture a child Vitest runs', changed: 'src/test/sharedWorker/skipped.fixture.ts', reader: 'src/test/sharedWorkerRunner.test.ts' },
];

// Inputs every file of one project reads: its runner and what the runner
// imports. Vitest follows a project's runner itself.
const PROJECT_INPUTS = [
  { changed: 'src/test/sharedWorkerRunner.ts', project: 'unit' },
  { changed: 'src/test/sharedWorker.ts', project: 'unit' },
  { changed: 'src/test/sharedGlobals.ts', project: 'unit' },
];

// Inputs every test file reads without an import: a change to the
// frontend's own runs every file, and another package's runs none.
const TRIGGERS = [
  { changed: 'package.json', all: true },
  { changed: 'pnpm-lock.yaml', all: true },
  { changed: 'tsconfig.json', all: true },
  { changed: 'vitest.config.ts', all: true },
  { changed: 'src/test/config/testResets.ts', all: true },
  { changed: '../package.json', all: false },
  { changed: '../e2e/package.json', all: false },
  { changed: '../pnpm-lock.yaml', all: false },
  { changed: '../e2e/tsconfig.json', all: false },
  { changed: '../e2e/pnpm-lock.yaml', all: false },
];

// The scratch repository: a frontend with this script, sources that read
// files inside and outside it, and files no test reads.
const FILES = {
  'frontend/src/lib/a.ts': 'export const a = 1;\n',
  'frontend/src/lib/a.test.ts': "import { a } from './a';\nvoid a;\n",
  'frontend/src/lib/old.test.ts': 'export {};\n',
  'frontend/src/lib/fixture.test.ts': 'export const fixture = 1;\n',
  'frontend/src/lib/parent.test.ts': "import './fixture.test';\n",
  'frontend/src/lib/probes.test.ts': "const probes = import.meta.glob('../../../scripts/perfprobe/**/*.mjs');\nvoid probes;\n",
  'frontend/src/lib/goSource.test.ts': "import source from '../../../internal/store/threads.go?raw';\nvoid source;\n",
  'scripts/perfprobe/ab.mjs': 'export {};\n',
  'scripts/release.sh': 'true\n',
  'internal/store/threads.go': 'package store\n',
  'docs/decisions.md': '# Decisions\n',
};

// Each case starts from the base commit. `committed` removes paths in a
// commit, `staged` with `git rm` alone, `unstaged` from the working tree;
// `renamed` moves a path in a commit.
const DELETIONS = [
  { kind: 'a deleted doc no test reads', committed: ['docs/decisions.md'], whole: false },
  { kind: 'a deleted script no test reads', committed: ['scripts/release.sh'], whole: false },
  { kind: 'a deleted file a glob reads', committed: ['scripts/perfprobe/ab.mjs'], whole: true },
  { kind: 'a renamed file a raw import reads', renamed: ['internal/store/threads.go', 'internal/store/thread_rows.go'], whole: true },
  { kind: 'a deleted source under frontend/', committed: ['frontend/src/lib/a.ts'], whole: true },
  { kind: 'a deleted test file no source imports', committed: ['frontend/src/lib/old.test.ts'], whole: false },
  { kind: 'a deleted test file another test imports', committed: ['frontend/src/lib/fixture.test.ts'], whole: true },
  { kind: 'a staged deletion a glob reads', staged: ['scripts/perfprobe/ab.mjs'], whole: true },
  { kind: 'an unstaged deletion a glob reads', unstaged: ['scripts/perfprobe/ab.mjs'], whole: true },
];

const failures = [];
const started = performance.now();

function git(repo, ...args) {
  return execFileSync('git', ['-C', repo, '-c', 'user.name=check', '-c', 'user.email=check@example.invalid', ...args], { encoding: 'utf8' });
}

const scratch = mkdtempSync(join(tmpdir(), 'vitest-changed-check-'));
try {
  for (const [path, text] of Object.entries(FILES)) {
    mkdirSync(dirname(join(scratch, path)), { recursive: true });
    writeFileSync(join(scratch, path), text);
  }
  cpSync(join(FRONTEND, 'scripts/vitest-changed.mjs'), join(scratch, 'frontend/scripts/vitest-changed.mjs'));
  git(scratch, 'init', '-q', '-b', 'main');
  git(scratch, 'add', '-A');
  git(scratch, 'commit', '-q', '-m', 'base');
  for (const { kind, committed, staged, unstaged, renamed, whole } of DELETIONS) {
    git(scratch, 'checkout', '-q', '-B', 'change', 'main');
    if (committed) git(scratch, 'rm', '-q', ...committed);
    if (renamed) git(scratch, 'mv', ...renamed);
    if (committed || renamed) git(scratch, 'commit', '-q', '-m', kind);
    if (staged) git(scratch, 'rm', '-q', ...staged);
    if (unstaged) for (const path of unstaged) rmSync(join(scratch, path));
    const printed = execFileSync('node', ['frontend/scripts/vitest-changed.mjs', 'unit', 'main', '--print'], { cwd: scratch, encoding: 'utf8' }).trim();
    const ranWhole = !printed.includes('--changed main');
    console.log(`${kind}: ${printed.split('\n').join(' / ')}`);
    if (ranWhole !== whole) failures.push(`${kind}: expected ${whole ? 'a whole run' : 'a --changed selection'}`);
    git(scratch, 'reset', '-q', '--hard', 'main');
  }
} finally {
  rmSync(scratch, { recursive: true, force: true });
}

const vitest = await createVitest('test', { watch: false, project: ['unit', 'unit-isolated'] }, {}, {});
try {
  for (const { kind, changed, reader } of READERS) {
    // The list `--changed` hands the resolver (VitestSpecifications.filterTestsBySource).
    vitest.config.related = [resolve(FRONTEND, changed)];
    const selected = (await vitest.getRelevantTestSpecifications()).map((spec) => spec.moduleId);
    const found = selected.includes(resolve(FRONTEND, reader));
    console.log(`${kind}: a change to ${changed} selects ${selected.length} files, ${found ? 'including' : 'NOT including'} ${reader}`);
    if (!found) failures.push(`${kind}: a change to ${changed} does not select ${reader}`);
  }
  vitest.config.related = undefined;
  const everySpec = await vitest.getRelevantTestSpecifications();
  const total = everySpec.length;
  for (const { changed, project } of PROJECT_INPUTS) {
    const projectTotal = everySpec.filter((spec) => spec.project.name === project).length;
    vitest.config.related = [resolve(FRONTEND, changed)];
    const selected = (await vitest.getRelevantTestSpecifications()).filter((spec) => spec.project.name === project).length;
    console.log(`a change to ${changed} selects ${selected} of the ${projectTotal} ${project} files`);
    if (selected !== projectTotal) failures.push(`a change to ${changed} selects ${selected} of the ${projectTotal} ${project} files, expected all`);
  }
  vitest.config.related = undefined;
  for (const { changed, all } of TRIGGERS) {
    vitest.config.related = [resolve(FRONTEND, changed)];
    const selected = (await vitest.getRelevantTestSpecifications()).length;
    console.log(`a change to ${changed} selects ${selected} of ${total} files`);
    if ((selected === total) !== all || (!all && selected !== 0)) {
      failures.push(`a change to ${changed} selects ${selected} of ${total} files, expected ${all ? 'all' : 'none'}`);
    }
  }
} finally {
  await vitest.close();
}

console.log(`checked in ${((performance.now() - started) / 1000).toFixed(1)} s`);
if (failures.length > 0) {
  console.error(failures.join('\n'));
  process.exit(1);
}
