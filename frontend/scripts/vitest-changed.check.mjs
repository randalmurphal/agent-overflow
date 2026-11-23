#!/usr/bin/env node
// Checks that a change to a file selects the tests that read it, the way
// `vitest-changed.mjs` selects them, for one reader of each kind: a raw
// import of a file that is not a module, a glob, a file outside frontend/,
// and a child-process workload, asked of Vitest's own related-files
// resolver; that a change to an input every test reads runs every file
// only when it is the frontend's own; and that a deletion through the script
// runs the projects whole.
//
//   cd frontend
//   ../bin/ao-netns node scripts/vitest-changed.check.mjs
import { resolve } from 'node:path';
import { createVitest } from 'vitest/node';
import { removedPaths, testReads, wholeRunReason } from './vitest-changed.mjs';

const FRONTEND = resolve(import.meta.dirname, '..');
const REPO = resolve(FRONTEND, '..');

const READERS = [
  { kind: 'raw import of a file that is not a module', changed: 'index.html', reader: 'src/indexHtmlNoInlineScript.test.ts' },
  { kind: 'glob', changed: 'src/styles/syntax.css', reader: 'src/lib/styleInvalidation.test.ts' },
  { kind: 'file outside frontend/', changed: '../internal/stringsx/findfold_table.go', reader: 'src/lib/utils/findMatch.test.ts' },
  { kind: 'child-process workload', changed: 'src/lib/markdown/parser/index.ts', reader: 'src/lib/markdown/parseBlockRetention.test.ts' },
];

// Inputs every test file reads without an import: a change to the
// frontend's own runs every file, and another package's runs none.
const TRIGGERS = [
  { changed: 'pnpm-lock.yaml', all: true },
  { changed: 'tsconfig.json', all: true },
  { changed: 'src/test/config/testResets.ts', all: true },
  { changed: '../pnpm-lock.yaml', all: false },
  { changed: '../e2e/tsconfig.json', all: false },
  { changed: '../e2e/pnpm-lock.yaml', all: false },
];

const DELETIONS = [
  { kind: 'deleted file a glob reads', nameStatus: 'D\0scripts/perfprobe/ab.mjs\0', whole: true },
  { kind: 'renamed file a raw import reads', nameStatus: 'R100\0internal/store/threads.go\0internal/store/thread_rows.go\0', whole: true },
  { kind: 'deleted file under frontend/', nameStatus: 'D\0frontend/src/lib/utils/htmlFile.ts\0', whole: true },
  { kind: 'deleted file no test reads', nameStatus: 'D\0docs/decisions.md\0M\0frontend/src/app.css\0', whole: false },
];

const failures = [];
const started = performance.now();

const reads = testReads(REPO);
for (const { kind, nameStatus, whole } of DELETIONS) {
  const reason = wholeRunReason(removedPaths(nameStatus), reads);
  console.log(`${kind}: ${reason ?? 'selected by --changed'}`);
  if ((reason !== null) !== whole) failures.push(`${kind}: expected ${whole ? 'a whole run' : 'a --changed selection'}`);
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
  const total = (await vitest.getRelevantTestSpecifications()).length;
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
