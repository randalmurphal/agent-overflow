// Plans a CI run: which of ci.yml's jobs it runs, from the event and, for a
// pull request, the paths it changes, and afterwards whether the run
// passed. These rules live only here; docs/architecture/development.md
// points at this file.
//
//   node scripts/ci-suites.mjs plan EVENT [PATHS_FILE]
//       (the toolchain job) prints plan=JSON for GITHUB_OUTPUT: every job
//       of ci.yml but toolchain and ci-result, each true or false;
//       e2e-grep-invert=, the Playwright --grep-invert pattern of the
//       tooling specs the change cannot reach, empty to run every spec; and
//       vitest-checks=JSON: each check of the Vitest tooling (vitestChecks
//       below), true or false, which a step of the vitest job reads. A pull
//       request needs PATHS_FILE; a push or a manual run runs every suite,
//       every spec and every check.
//   node scripts/ci-suites.mjs verdict   (the CI result job, with PLAN and
//       NEEDS in the environment) fails unless every job the plan selected
//       succeeded and every job it excluded was skipped.
//
// Each job's `if` reads its own key of the plan, and the verdict holds each
// job to it, so a job the plan selected cannot pass the run by being
// skipped; scripts/ci-suites.test.mjs holds ci.yml to that shape.
//
// The Go test shards always run: the Go test cache decides which packages
// a change reaches, through the files each test reads. A change to the
// Playwright suite runs all of it: Playwright's --only-changed misses a
// spec that reaches a changed helper through another helper
// (review-diff-reading.spec.ts through review-diff-reading-flow.ts to
// review-comment-actions-flow.ts). The exceptions are the checks of the
// suites' own tooling (toolingSpecs and vitestChecks below), which a change
// skips when none of its paths can change their result.
//
// Every path falls in the first class that matches it, and a suite runs
// when the change holds a path of a class it reads. A path no class
// matches runs every suite. Each class lists only what a suite provably
// does not read, so a new kind of file costs time, never coverage.
import { readFileSync } from 'node:fs';
import { dirname, join, posix, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { PROJECT_INPUTS, READERS, TRIGGERS } from '../frontend/scripts/vitest-changed.check.mjs';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');

// harnessTestPackages are the packages whose test binaries the Playwright
// suite runs (E2E_TEST_PACKAGES in build/test-inputs.mk), whose test files
// are therefore Playwright inputs.
export function harnessTestPackages(declaration = readFileSync(join(root, 'build/test-inputs.mk'), 'utf8')) {
  const line = /^E2E_TEST_PACKAGES :=(.*)$/m.exec(declaration);
  if (!line) throw new Error('build/test-inputs.mk no longer sets E2E_TEST_PACKAGES');
  const packages = line[1].trim().split(/\s+/).filter(Boolean).map(pkg => pkg.replace(/^\.\//, ''));
  if (packages.length === 0 || packages.some(pkg => !/^[\w./-]+$/.test(pkg) || pkg.includes('...'))) {
    throw new Error(`E2E_TEST_PACKAGES names no plain package directories: ${line[1].trim()}`);
  }
  return packages;
}

// classify names a path's class. Searched before each class was added,
// over every suite's sources for what they read outside their own
// directory: no Go package embeds, and no Vitest, Playwright or lint source
// reads, a Markdown file under docs/ or a guide file; Vitest and
// Playwright read no _test.go file but those of
// the harness test packages; svelte-check and the bundle read nothing
// outside frontend/. Vitest and Playwright read testdata under internal/
// and cmd/, which falls through to "other". Markdown elsewhere is no prose
// class: Go embeds some (internal/workflow/starters/content, assets/).
export function classify(path, harnessPackages) {
  const base = path.slice(path.lastIndexOf('/') + 1);
  // The prose class, and the only one: Markdown under docs/ and the guide
  // files. No suite reads anything under docs/.
  if ((path.startsWith('docs/') && path.endsWith('.md')) || base === 'AGENTS.md' || base === 'CLAUDE.md' || path === 'README.md') return 'docs';
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
  // knip reads the Playwright suite's imports of frontend source.
  frontend: ['frontend', 'frontend-test', 'e2e'],
  vitest: ['frontend', 'frontend-test', 'go'],
  e2e: ['frontend', 'go', 'e2e'],
};

// upgradeLineage matches what the upgrade lineage check can break on: the
// store; the launcher's update commands the check migrates through
// (main_update*.go, __update-snapshot and __update-trial-run) and the
// reports they write (internal/supervise); the check, its corpus and its
// update driver (e2e/tests/update-launcher-helpers.ts); and the version
// bump.
const upgradeLineage = /^(internal\/store\/|internal\/supervise\/|main_update[^/]*\.go$|e2e\/tests\/upgrade-corpus|e2e\/tests\/update-launcher-helpers\.ts$|scripts\/release-upgrade-check\.sh$|build\/config\.yml$|frontend\/package\.json$)/;

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

// toolingSpecs names each Playwright spec that tests the suite's own
// tooling rather than the app, with the classes of path that cannot change
// its result. A pull request whose every path is in those classes, or is
// another gate spec, skips the spec in every shard; any other path, a new
// kind of file included, runs it. Another gate spec cannot reach it:
// Playwright refuses a test file that imports another test file of the
// run. A manual spec is no test file of the gate run, so it is not exempt.
export const toolingSpecs = {
  // A nested Playwright run, spawned with node, of tests/stalled-page/
  // against a synthetic page server: it boots no backend and opens no app
  // page. It reads e2e/ (the fixtures, src/page-stall.ts, Playwright's
  // version in e2e/package.json and the lockfile) and the browsers, which
  // are in class other.
  'e2e/tests/stalled-page-capture.spec.ts': ['docs', 'frontend', 'frontend-test', 'go', 'go-test'],
  // They take no page: the store, the update commands, the mock scenarios
  // and their e2e helpers decide them.
  'e2e/tests/upgrade-corpus-recovery.spec.ts': ['docs', 'frontend', 'frontend-test'],
  'e2e/tests/upgrade-corpus-coverage.spec.ts': ['docs', 'frontend', 'frontend-test'],
};

const gateSpec = path => /^e2e\/tests\/.+\.spec\.ts$/.test(path) && !path.endsWith('.manual.spec.ts');

// skippedSpecs lists the tooling specs a pull request with these paths
// cannot change.
export function skippedSpecs(paths, harnessPackages) {
  return Object.entries(toolingSpecs)
    .filter(([spec, classes]) => paths.every(path => path !== spec && (gateSpec(path) || classes.includes(classify(path, harnessPackages)))))
    .map(([spec]) => spec);
}

// grepInvert matches the tests of these spec files by the file part of
// the title Playwright greps (project, file, describes, title, tags, joined
// by spaces), or is empty.
export function grepInvert(specs) {
  if (specs.length === 0) return '';
  const files = specs.map(spec => spec.slice('e2e/tests/'.length).replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
  return `(^| )(${files.join('|')})( |$)`;
}

export function e2eGrepInvert(event, paths, harnessPackages) {
  return event === 'pull_request' ? grepInvert(skippedSpecs(paths, harnessPackages)) : '';
}

// vitestChecks names each check of the Vitest tooling that the vitest job
// runs as a step, with the paths that can change its result. A pull
// request runs a check when it runs the vitest job and changes one of
// those paths, or a path no class matches; a push or a manual run runs
// every check.
const fromFrontend = path => posix.normalize(`frontend/${path}`);
const selectionInputs = new Set([
  ...READERS.flatMap(({ changed, reader }) => [changed, reader]),
  ...PROJECT_INPUTS.map(({ changed }) => changed),
  ...TRIGGERS.filter(({ all }) => all).map(({ changed }) => changed),
].map(fromFrontend));
export const vitestChecks = {
  // frontend/scripts/vitest-changed.check.mjs: the files its tables name,
  // the selector and the check, the TypeScript config, and the Vitest
  // plugins in src/test/config/ through which the resolver sees what a
  // test reads. It does not select a change to a file between a reader and
  // the file it reads (parseBlockRetention.workload.ts), or a frontend file
  // that starts importing a file the check expects to select no test
  // (../package.json, ../e2e/package.json).
  'test-selection': path => selectionInputs.has(path) ||
    /^frontend\/(scripts\/vitest-changed(\.check)?\.mjs$|tsconfig[^/]*\.json$|src\/test\/config\/)/.test(path),
  // frontend/scripts/check-stalled-page-watchdog.mjs: the check, the
  // watchdog, its fixtures and child config, the config whose registration
  // it checks, the Vitest and Playwright versions, and the TypeScript
  // config. The browsers come from paths no class matches.
  'stall-watchdog': path =>
    /^frontend\/(scripts\/check-stalled-page-watchdog\.mjs$|src\/test\/stalledPageWatchdog\.ts$|src\/test\/stalledPage\/|vitest\.config\.ts$|package\.json$|pnpm-lock\.yaml$|tsconfig[^/]*\.json$)/.test(path),
};

export function vitestCheckRuns(event, paths, harnessPackages) {
  const pullRequest = event === 'pull_request';
  const runsVitest = !pullRequest || suites(paths, harnessPackages).vitest;
  return Object.fromEntries(Object.entries(vitestChecks).map(([check, reads]) => [check,
    runsVitest && (!pullRequest || paths.some(path => classify(path, harnessPackages) === 'other' || reads(path)))]));
}

export const everything = { 'go-checks': true, 'go-lint': true, frontend: true, vitest: true, e2e: true, 'upgrade-lineage': true };

// jobs maps each job of ci.yml the plan decides, but toolchain and
// ci-result, to whether a run of event with these suites runs it.
export const jobs = {
  'go-checks': (suite) => suite['go-checks'],
  // The macOS build reads what the lint does: Go code.
  'build-macos': (suite) => suite['go-lint'],
  'go-lint': (suite) => suite['go-lint'],
  frontend: (suite) => suite.frontend,
  'go-test': () => true,
  vitest: (suite) => suite.vitest,
  'harness-build': (suite, event) => suite.e2e || (suite['upgrade-lineage'] && event !== 'push'),
  e2e: (suite) => suite.e2e,
  // A push to main does not repeat the lineage check its pull request ran.
  'upgrade-lineage': (suite, event) => suite['upgrade-lineage'] && event !== 'push',
  'dependency-review': (suite, event) => event === 'pull_request',
  'cache-cleanup': (suite, event) => event === 'push',
};

const events = ['pull_request', 'push', 'workflow_dispatch'];

export function plan(event, paths, harnessPackages) {
  if (!events.includes(event)) throw new Error(`no plan for event ${event}; ci.yml runs on ${events.join(', ')}`);
  if (event === 'pull_request' && paths === undefined) throw new Error('a pull request plan needs its changed paths');
  const suite = event === 'pull_request' ? suites(paths, harnessPackages) : everything;
  const result = Object.fromEntries(Object.entries(jobs).map(([job, runs]) => [job, runs(suite, event)]));
  validatePlan(result);
  return result;
}

// validatePlan fails unless plan names exactly the jobs, each true or false.
export function validatePlan(plan) {
  const keys = Object.keys(plan ?? {}).sort();
  const want = Object.keys(jobs).sort();
  if (keys.join() !== want.join()) throw new Error(`the plan names ${keys.join(', ') || 'no jobs'}, want ${want.join(', ')}`);
  for (const [job, value] of Object.entries(plan)) {
    if (typeof value !== 'boolean') throw new Error(`the plan's ${job} is ${JSON.stringify(value)}, not true or false`);
  }
}

// verdict answers what is wrong with a run, one line per job, or nothing:
// toolchain succeeded, every job the plan selected succeeded and every job
// it excluded was skipped.
export function verdict(planText, needs) {
  if (needs.toolchain?.result !== 'success') return [`toolchain: ${needs.toolchain?.result ?? 'missing'}; there is no plan to hold the run to`];
  let selected;
  try {
    selected = JSON.parse(planText);
    validatePlan(selected);
  } catch (error) {
    return [`the toolchain job published no valid plan: ${error.message}`];
  }
  const problems = [];
  for (const [job, runs] of Object.entries(selected)) {
    const result = needs[job]?.result ?? 'missing from the result job\'s needs';
    const want = runs ? 'success' : 'skipped';
    if (result !== want) problems.push(`${job}: ${result}, the plan ${runs ? 'selected it' : 'excluded it'}`);
  }
  for (const job of Object.keys(needs)) {
    if (job !== 'toolchain' && !(job in selected)) problems.push(`${job}: the result job needs it but the plan does not decide it`);
  }
  return problems;
}

function main([command, ...args]) {
  if (command === 'plan' && (args.length === 1 || args.length === 2)) {
    const [event, pathsFile] = args;
    const paths = pathsFile === undefined ? undefined : readFileSync(pathsFile, 'utf8').split('\n').filter(Boolean);
    const harness = harnessTestPackages();
    console.log(`plan=${JSON.stringify(plan(event, paths, harness))}`);
    console.log(`e2e-grep-invert=${e2eGrepInvert(event, paths, harness)}`);
    console.log(`vitest-checks=${JSON.stringify(vitestCheckRuns(event, paths, harness))}`);
    return;
  }
  if (command === 'verdict' && args.length === 0) {
    const needs = JSON.parse(process.env.NEEDS ?? '{}');
    for (const [job, { result }] of Object.entries(needs)) console.log(`${job}: ${result}`);
    const problems = verdict(process.env.PLAN ?? '', needs);
    if (problems.length > 0) throw new Error(problems.join('; '));
    return;
  }
  throw new Error('usage: ci-suites.mjs plan EVENT [PATHS_FILE] | verdict');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    console.error(`::error title=CI plan::${error.message}`);
    process.exitCode = 1;
  }
}
