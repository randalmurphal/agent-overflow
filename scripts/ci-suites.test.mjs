import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { e2eGrepInvert, grepInvert, harnessTestPackages, jobs, plan, skippedSpecs, suites, toolingSpecs, verdict, vitestCheckRuns, vitestChecks } from './ci-suites.mjs';
import { PROJECT_INPUTS, READERS, TRIGGERS } from '../frontend/scripts/vitest-changed.check.mjs';

const harness = harnessTestPackages('# packages\nE2E_TEST_PACKAGES := ./internal/frontendclient ./internal/store\n');
const run = paths => suites(paths, harness);
const none = { 'go-checks': false, 'go-lint': false, frontend: false, vitest: false, e2e: false, 'upgrade-lineage': false };

test('the run declaration names the harness test packages', () => {
  assert.deepEqual(harness, ['internal/frontendclient', 'internal/store']);
  assert.ok(harnessTestPackages().length > 0);
  assert.throws(() => harnessTestPackages('E2E_BROWSERS := x\n'), /no longer sets E2E_TEST_PACKAGES/);
  assert.throws(() => harnessTestPackages('E2E_TEST_PACKAGES := ./internal/...\n'), /no plain package directories/);
});

test('each change runs the suites that read it', () => {
  const cases = [
    [['docs/architecture/development.md', 'internal/app/AGENTS.md', 'frontend/CLAUDE.md', 'README.md'], none],
    [['e2e/playwright.config.ts'], { ...none, frontend: true, e2e: true }],
    [['e2e/package.json'], { ...none, frontend: true, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'e2e/src/harness.ts'], { ...none, frontend: true, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'docs/specs/x.md'], { ...none, frontend: true, e2e: true }],
    [['frontend/src/lib/components/chat/AGENTS.md', 'internal/app/CLAUDE.md'], none],
    [['e2e/tests/pane-find.spec.ts', 'e2e/scripts/android-smoke.sh'], { ...none, frontend: true, e2e: true }],
    [['frontend/src/lib/a.test.ts', 'frontend/src/lib/b.browser.test.ts', 'frontend/src/test/setup.ts', 'frontend/src/lib/c.manual.ts'],
      { ...none, frontend: true, vitest: true }],
    [['frontend/src/lib/stores/x.svelte.ts'], { ...none, 'go-checks': true, frontend: true, vitest: true, e2e: true }],
    [['frontend/bindings/agent-overflow/app.ts'], { ...none, 'go-checks': true, frontend: true, vitest: true, e2e: true }],
    [['internal/app/app_test.go'], { ...none, 'go-checks': true, 'go-lint': true }],
    [['internal/store/store_test.go'], { ...none, 'go-checks': true, 'go-lint': true, vitest: true, e2e: true, 'upgrade-lineage': true }],
    [['internal/frontendclient/client_test.go'], { ...none, 'go-checks': true, 'go-lint': true, vitest: true, e2e: true }],
    [['internal/frontendclient/sub/x_test.go'], { ...none, 'go-checks': true, 'go-lint': true }],
    [['internal/app/app.go'], { ...none, 'go-checks': true, 'go-lint': true, vitest: true, e2e: true }],
    [['go.sum'], { ...none, 'go-checks': true, 'go-lint': true, vitest: true, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'internal/app/app.go'], { ...none, 'go-checks': true, 'go-lint': true, frontend: true, vitest: true, e2e: true }],
    [[], none],
  ];
  for (const [paths, want] of cases) assert.deepEqual(run(paths), want, paths.join(' '));
});

test('a path no class matches runs everything', () => {
  for (const path of ['Makefile', 'internal/app/testdata/agent_message_labels.json', 'internal/harness/scenario/library/x.json',
    // Playwright replays these captures; Go embeds the starter prompts.
    'internal/provider/wirecaptures/claude/cron_create_fire_20261010.ndjson', 'internal/workflow/starters/content/build-and-validate/implement.md',
    'scripts/perfprobe/probe', '.github/workflows/ci.yml', 'ci/Dockerfile', 'mobile/package.json', 'build/config.yml']) {
    const result = run(['docs/x.md', path]);
    assert.deepEqual({ ...result, 'upgrade-lineage': false },
      { 'go-checks': true, 'go-lint': true, frontend: true, vitest: true, e2e: true, 'upgrade-lineage': false }, path);
  }
});

test('the upgrade lineage check runs for the store, its corpus, the check and the version', () => {
  for (const path of ['internal/store/migrations.go', 'e2e/tests/upgrade-corpus-config.ts', 'scripts/release-upgrade-check.sh',
    'build/config.yml', 'frontend/package.json', 'main_update.go', 'main_update_apply.go', 'internal/supervise/events.go',
    'e2e/tests/update-launcher-helpers.ts']) {
    assert.equal(run([path])['upgrade-lineage'], true, path);
  }
  for (const path of ['internal/storex/x.go', 'e2e/tests/pane-find.spec.ts', 'frontend/package-lock.json', 'main_updatex/x.go', 'main.go']) {
    assert.equal(run([path])['upgrade-lineage'], false, path);
  }
});

const stalled = 'e2e/tests/stalled-page-capture.spec.ts';
const recovery = 'e2e/tests/upgrade-corpus-recovery.spec.ts';
const coverage = 'e2e/tests/upgrade-corpus-coverage.spec.ts';
const skipped = paths => skippedSpecs(paths, harness);

test('the tooling specs are the Playwright specs the table names', () => {
  assert.deepEqual(Object.keys(toolingSpecs).sort(), [stalled, coverage, recovery].sort());
  for (const spec of Object.keys(toolingSpecs)) assert.ok(existsSync(new URL(`../${spec}`, import.meta.url)), `${spec} exists`);
});

test('a pull request skips a tooling spec only when no path it changes can change the spec', () => {
  const cases = [
    [['frontend/src/lib/stores/x.svelte.ts', 'frontend/src/lib/a.test.ts', 'frontend/package.json', 'docs/x.md'], [stalled, recovery, coverage]],
    // Another gate spec is in no spec's imports.
    [['e2e/tests/pane-find.spec.ts', 'e2e/tests/compact-pane-find.spec.ts', 'frontend/src/lib/x.ts'], [stalled, recovery, coverage]],
    [['internal/app/app.go', 'internal/app/app_test.go', 'internal/store/store_test.go', 'cmd/ao-harness-e2e/main.go', 'go.sum'], [stalled]],
    [['internal/store/migrations.go', 'e2e/tests/pane-find.spec.ts'], [stalled]],
  ];
  for (const [paths, want] of cases) assert.deepEqual(skipped(paths).sort(), want.sort(), paths.join(' '));
});

test('each path that can change the stalled-page capture spec runs it', () => {
  for (const path of [stalled, 'e2e/package.json', 'e2e/pnpm-lock.yaml', 'e2e/src/page-stall.ts', 'e2e/src/harness.ts',
    'e2e/tests/base-test.ts', 'e2e/tests/fixtures.ts', 'e2e/tests/stalled-page/blocked-page.fixture.ts', 'e2e/tests/stalled-page/blocked-page.ts',
    'e2e/tests/stalled-page/playwright.config.ts', 'e2e/playwright.config.ts', 'e2e/test-files.ts', 'e2e/tests/x.manual.spec.ts',
    'build/test-inputs.mk', 'scripts/playwright-browsers.sh', 'ci/Dockerfile', 'Makefile']) {
    assert.ok(!skipped(['frontend/src/lib/x.ts', path]).includes(stalled), path);
  }
});

test('each path outside frontend/ and the other specs runs the upgrade-corpus specs', () => {
  for (const spec of [recovery, coverage]) {
    for (const path of [spec, 'internal/store/migrations.go', 'internal/app/app.go', 'main_update.go', 'internal/store/store_test.go', 'go.sum',
      'internal/harness/scenario/library/x.json', 'build/config.yml', 'e2e/tests/upgrade-corpus-config.ts', 'e2e/tests/update-launcher-helpers.ts',
      'e2e/tests/fixtures.ts', 'e2e/src/harness.ts', 'e2e/package.json', 'e2e/tests/upgrade-corpus.manual.spec.ts', 'Makefile']) {
      assert.ok(!skipped(['frontend/src/lib/x.ts', path]).includes(spec), `${spec}: ${path}`);
    }
  }
});

test('the skip pattern matches the skipped files in the title Playwright greps, and only on a pull request', () => {
  const pattern = new RegExp(grepInvert([stalled, coverage]), 'gi');
  const matches = title => { pattern.lastIndex = 0; return pattern.test(title); };
  assert.ok(matches(' desktop stalled-page-capture.spec.ts a failed test attaches the stack'));
  assert.ok(matches(' mobile-webkit stalled-page-capture.spec.ts a failed test'));
  assert.ok(matches(' desktop upgrade-corpus-coverage.spec.ts the corpus and the top-up'));
  for (const title of [' desktop upgrade-corpus-recovery.spec.ts t', ' compact compact-stalled-page-capture.spec.ts t',
    ' desktop stalled-page-capturexspec.ts t', ' desktop pane-find.spec.ts mentions stalled-page-capture.spec.tsx']) {
    assert.ok(!matches(title), title);
  }
  assert.equal(grepInvert([]), '');
  const frontendOnly = ['frontend/src/lib/x.ts'];
  assert.equal(e2eGrepInvert('pull_request', frontendOnly, harness), grepInvert([stalled, recovery, coverage]));
  assert.equal(e2eGrepInvert('pull_request', ['Makefile'], harness), '');
  assert.equal(e2eGrepInvert('push', undefined, harness), '');
  assert.equal(e2eGrepInvert('workflow_dispatch', undefined, harness), '');
});

// Where each Vitest tooling check runs, and the package script a person
// runs it with.
const vitestCheckSteps = {
  'test-selection': { leg: 'unit', script: 'check:test-selection', file: 'frontend/scripts/vitest-changed.check.mjs' },
  'stall-watchdog': { leg: 'browser', script: 'check:stall-watchdog', file: 'frontend/scripts/check-stalled-page-watchdog.mjs' },
};
const checksRun = paths => vitestCheckRuns('pull_request', paths, harness);
const selects = (check, path) => checksRun(['frontend/src/lib/x.ts', path])[check];

test('the Vitest tooling checks are the ones the table names, and every run but a pull request runs them all', () => {
  assert.deepEqual(Object.keys(vitestChecks).sort(), Object.keys(vitestCheckSteps).sort());
  for (const event of ['push', 'workflow_dispatch']) {
    assert.deepEqual(vitestCheckRuns(event, undefined, harness), { 'test-selection': true, 'stall-watchdog': true }, event);
  }
  const printed = execFileSync('node', ['scripts/ci-suites.mjs', 'plan', 'push'], { cwd: new URL('..', import.meta.url), encoding: 'utf8' });
  assert.match(printed, /^vitest-checks=\{"test-selection":true,"stall-watchdog":true\}$/m);
});

test('each path that can change the affected-test selection check runs it', () => {
  // The check's tables name its readers, the files they read, and the
  // inputs every file or every file of a project reads.
  const named = [...READERS.flatMap(({ changed, reader }) => [changed, reader]), ...PROJECT_INPUTS.map(({ changed }) => changed),
    ...TRIGGERS.filter(({ all }) => all).map(({ changed }) => changed)];
  assert.ok(named.includes('../internal/stringsx/findfold_table.go'));
  for (const path of [...named.map(path => path.startsWith('../') ? path.slice(3) : `frontend/${path}`),
    'frontend/scripts/vitest-changed.mjs', 'frontend/scripts/vitest-changed.check.mjs', 'frontend/tsconfig.json', 'frontend/tsconfig.node.json',
    'frontend/src/test/config/rawImportsPlugin.ts', 'Makefile', 'build/test-inputs.mk', '.github/workflows/ci.yml']) {
    assert.equal(selects('test-selection', path), true, path);
  }
});

test('each path that can change the stalled-page watchdog check runs it', () => {
  for (const path of ['frontend/scripts/check-stalled-page-watchdog.mjs', 'frontend/src/test/stalledPageWatchdog.ts',
    'frontend/src/test/stalledPage/blocked-test.fixture.ts', 'frontend/src/test/stalledPage/vitest.config.ts', 'frontend/vitest.config.ts',
    'frontend/package.json', 'frontend/pnpm-lock.yaml', 'frontend/tsconfig.json', 'build/test-inputs.mk', 'scripts/playwright-browsers.sh',
    'ci/Dockerfile', '.github/actions/setup/action.yml']) {
    assert.equal(selects('stall-watchdog', path), true, path);
  }
});

test('a pull request skips a Vitest tooling check when no path it changes can change the check', () => {
  const none = { 'test-selection': false, 'stall-watchdog': false };
  for (const path of ['frontend/src/test/mocks/bindings-app.ts', 'frontend/src/test/helpers/nodeWorkload.ts', 'frontend/src/test/setup.ts',
    'frontend/vite.config.ts', 'frontend/src/lib/a.test.ts', 'frontend/scripts/bundleId.ts', 'internal/app/app.go', 'internal/stringsx/findfold.go']) {
    assert.deepEqual(checksRun(['frontend/src/lib/x.ts', path]), none, path);
  }
  assert.equal(selects('stall-watchdog', 'frontend/src/test/config/rawImportsPlugin.ts'), false);
  assert.equal(selects('test-selection', 'frontend/src/test/stalledPage/quick.fixture.ts'), false);
  // A change the vitest job does not run for runs none of its checks.
  for (const paths of [['docs/x.md'], ['internal/app/app_test.go'], ['e2e/package.json']]) assert.deepEqual(checksRun(paths), none, paths.join(' '));
});

test('the vitest job runs each tooling check in its leg, as the plan says, with the package script the check names', () => {
  const found = ciJobs();
  assert.match(found.toolchain.outputs, /^      vitest-checks: \$\{\{ steps\.plan\.outputs\.vitest-checks \}\}$/m);
  const scripts = JSON.parse(readFileSync(new URL('../frontend/package.json', import.meta.url), 'utf8')).scripts;
  for (const [check, { leg, script, file }] of Object.entries(vitestCheckSteps)) {
    const steps = found.vitest.steps.filter(step => step.includes(`pnpm run ${script}`));
    assert.equal(steps.length, 1, `the vitest job runs ${script} once`);
    assert.match(steps[0], new RegExp(`^        if: matrix\\.project == '${leg}' && fromJSON\\(needs\\.toolchain\\.outputs\\.vitest-checks\\)\\['${check}'\\]$`, 'm'), check);
    assert.match(steps[0], new RegExp(`^        working-directory: frontend\\n        run: \\.\\./bin/ao-netns pnpm run ${script}$`, 'm'), check);
    assert.equal(scripts[script], `node scripts/${file.slice('frontend/scripts/'.length)}`, script);
    assert.ok(readFileSync(new URL(`../${file}`, import.meta.url), 'utf8').includes(`../bin/ao-netns pnpm run ${script}\n`), `${file} names ${script}`);
  }
  for (const [id, job] of Object.entries(found)) {
    if (id !== 'vitest' && id !== 'toolchain') assert.doesNotMatch(job.steps.join('\n') + job.env, /vitest-checks/, id);
  }
});

test('the affected-test selection check loads with node: builtins alone, as the toolchain job imports it', () => {
  const source = readFileSync(new URL('../frontend/scripts/vitest-changed.check.mjs', import.meta.url), 'utf8');
  const imports = [...source.matchAll(/^(?:import\b[^'";]*?|export\b[^'";=]*?\bfrom\s*)['"]([^'"]+)['"]/gm)].map(([, specifier]) => specifier);
  assert.ok(imports.length > 0);
  assert.deepEqual(imports.filter(specifier => !specifier.startsWith('node:')), []);
});

const all = Object.fromEntries(Object.keys(jobs).map(job => [job, true]));

test('the plan names every job, from the event', () => {
  assert.deepEqual(plan('push', undefined, harness), { ...all, 'upgrade-lineage': false, 'dependency-review': false });
  assert.deepEqual(plan('workflow_dispatch', undefined, harness), { ...all, 'dependency-review': false, 'cache-cleanup': false });
  const docsOnly = plan('pull_request', ['docs/x.md'], harness);
  assert.deepEqual(Object.entries(docsOnly).filter(([, runs]) => runs).map(([job]) => job), ['go-test', 'dependency-review']);
  const store = plan('pull_request', ['internal/store/migrations.go'], harness);
  assert.equal(store['upgrade-lineage'], true);
  assert.equal(store['harness-build'], true);
  assert.throws(() => plan('pull_request', undefined, harness), /needs its changed paths/);
  assert.throws(() => plan('schedule', undefined, harness), /no plan for event schedule/);
});

test('the verdict holds each job to the plan', () => {
  const selected = plan('pull_request', ['docs/x.md'], harness);
  const needs = { toolchain: { result: 'success' } };
  for (const [job, runs] of Object.entries(selected)) needs[job] = { result: runs ? 'success' : 'skipped' };
  assert.deepEqual(verdict(JSON.stringify(selected), needs), []);
  // A selected job that was skipped, by a typo in its if or a missing
  // output, fails the run, as a failed one does.
  assert.deepEqual(verdict(JSON.stringify(selected), { ...needs, 'go-test': { result: 'skipped' } }),
    ['go-test: skipped, the plan selected it']);
  assert.deepEqual(verdict(JSON.stringify(selected), { ...needs, e2e: { result: 'success' } }), ['e2e: success, the plan excluded it']);
  assert.match(verdict(JSON.stringify({ ...selected, e2e: 'yes' }), needs)[0], /not true or false/);
  assert.match(verdict('', needs)[0], /no valid plan/);
  assert.match(verdict(JSON.stringify(selected), { ...needs, toolchain: { result: 'failure' } })[0], /toolchain: failure/);
  const { vitest: _, ...withoutVitest } = needs;
  assert.deepEqual(verdict(JSON.stringify(selected), withoutVitest), ["vitest: missing from the result job's needs, the plan excluded it"]);
});

// workflowJobs reads a workflow's jobs: each one's id, needs, if, env,
// permissions and steps, by the indentation ci.yml keeps.
function workflowJobs(file) {
  const text = readFileSync(new URL(`../.github/workflows/${file}`, import.meta.url), 'utf8');
  const body = text.slice(text.indexOf('\njobs:\n'));
  const found = {};
  for (const block of body.split(/\n(?=  [a-z0-9-]+:\n)/).slice(1)) {
    const id = /^  ([a-z0-9-]+):/.exec(block)[1];
    const needs = /\n    needs: (\[[^\]]*\]|[a-z0-9-]+)\n/.exec(block)?.[1] ?? '';
    const section = name => new RegExp(`\\n    ${name}:\\n((?:      .*\\n|\\s*\\n)*)`).exec(block + '\n')?.[1] ?? '';
    found[id] = {
      needs: needs.replace(/[[\]\s]/g, '').split(',').filter(Boolean),
      if: /\n    if: (.*)\n/.exec(block)?.[1] ?? '',
      env: section('env'),
      permissions: section('permissions'),
      outputs: section('outputs'),
      steps: section('steps').split(/\n(?=      - )/).filter(step => step.trim()),
    };
  }
  return found;
}
const ciJobs = () => workflowJobs('ci.yml');

test('every job ci.yml runs follows its own key of the plan, and the result job needs them all', () => {
  const found = ciJobs();
  const decided = Object.keys(found).filter(id => id !== 'toolchain' && id !== 'ci-result').sort();
  assert.deepEqual(decided, Object.keys(jobs).sort());
  for (const id of decided) {
    assert.ok(found[id].needs.includes('toolchain'), `${id} needs toolchain`);
    assert.ok(found[id].if.includes(`fromJSON(needs.toolchain.outputs.plan || '{}')['${id}']`), `${id}: if: ${found[id].if}`);
  }
  assert.deepEqual(found['ci-result'].needs.sort(), ['toolchain', ...decided].sort());
  // It runs whatever the others did, and it is the verdict that fails it.
  assert.equal(found['ci-result'].if, 'always()');
  assert.ok(found['ci-result'].steps.some(step => /run: node scripts\/ci-suites\.mjs verdict\n/.test(step + '\n')), 'ci-result runs the verdict');
});

test('every Playwright shard skips the specs the plan names, and only those', () => {
  const found = ciJobs();
  assert.match(found.toolchain.outputs, /^      e2e-grep-invert: \$\{\{ steps\.plan\.outputs\.e2e-grep-invert \}\}$/m);
  const runs = found.e2e.steps.filter(step => /run: bin\/ao-harness-e2e --shard=/.test(step));
  assert.equal(runs.length, 1);
  assert.match(runs[0], /run: bin\/ao-harness-e2e --shard=\$\{\{ matrix\.shard \}\}\/8 \$\{E2E_GREP_INVERT:\+"--grep-invert=\$E2E_GREP_INVERT"\}\n/);
  assert.match(runs[0], /^          E2E_GREP_INVERT: \$\{\{ needs\.toolchain\.outputs\.e2e-grep-invert \}\}$/m);
  for (const [id, job] of Object.entries(found)) {
    if (id !== 'e2e' && id !== 'toolchain') assert.doesNotMatch(job.steps.join('\n') + job.env, /grep-invert/, id);
  }
});

// launches names a step that starts the Playwright launcher, directly or
// through the script that does.
const launches = step => /bin\/ao-harness-e2e(?! --typecheck)|release-upgrade-check\.sh|make e2e(?!-browsers)/.test(step);

// runsArtifact is whether a job downloads the harness build's artifact.
const runsArtifact = job => job.steps.some(step => step.includes('uses: actions/download-artifact') && step.includes('name: harness-bin'));

test('a job that runs the harness build unpacks it first and runs it once', () => {
  const found = ciJobs();
  const runners = Object.keys(found).filter(id => runsArtifact(found[id]));
  assert.deepEqual(runners.sort(), ['e2e', 'upgrade-lineage']);
  for (const id of runners) {
    assert.ok(found[id].needs.includes('harness-build'), `${id} needs harness-build`);
    const unpack = found[id].steps.findIndex(step => step.includes('tar --zstd -xf'));
    const runs = found[id].steps.map((step, i) => launches(step) ? i : -1).filter(i => i >= 0);
    assert.ok(unpack >= 0, `${id} unpacks the harness build`);
    assert.equal(runs.length, 1, `${id} runs the launcher once: ${runs}`);
    assert.ok(runs[0] > unpack, `${id} runs the launcher at step ${runs[0]}, before the unpack at ${unpack}`);
  }
});

test('make e2e-browsers installs and runs nothing; the run targets run their own specs', () => {
  const dry = target => execFileSync('make', ['-s', '-n', target], { cwd: new URL('..', import.meta.url), encoding: 'utf8' });
  assert.doesNotMatch(dry('e2e-browsers'), /ao-harness-e2e/);
  const launch = target => dry(target).split('\n').filter(line => line.startsWith('bin/ao-harness-e2e'));
  const mobile = launch('e2e-mobile-browser');
  assert.equal(mobile.length, 1);
  assert.match(mobile[0], /tests\/compact-browser-lock\.spec\.ts$/);
  const full = launch('e2e');
  assert.equal(full.length, 1);
  assert.doesNotMatch(full[0], /\.spec\.ts/);
  // A local run has no change to select from: it runs every spec.
  assert.doesNotMatch(full[0], /grep-invert/);
});

test('only the jobs that run the typechecked artifact skip the typecheck, and the build checks before it packs', () => {
  const found = ciJobs();
  for (const [id, job] of Object.entries(found)) {
    const named = /AO_E2E_TYPECHECKED/.test(job.env) || job.steps.some(step => step.includes('AO_E2E_TYPECHECKED'));
    if (named) {
      assert.ok(job.needs.includes('harness-build') && runsArtifact(job), `${id} sets AO_E2E_TYPECHECKED without running the harness build's artifact`);
      assert.match(job.env, /^      AO_E2E_TYPECHECKED: "1"$/m, `${id} sets it at job level`);
    } else {
      assert.ok(!runsArtifact(job), `${id} runs the artifact and typechecks again`);
    }
  }
  const steps = found['harness-build'].steps;
  const check = steps.findIndex(step => step.includes('bin/ao-harness-e2e --typecheck'));
  const pack = steps.findIndex(step => step.includes('tar --zstd -cf'));
  assert.ok(check >= 0 && pack > check, `harness-build typechecks at step ${check} and packs at ${pack}`);
});

test('no job a pull request can run holds actions: write', () => {
  const writers = [];
  for (const file of readdirSync(new URL('../.github/workflows/', import.meta.url)).filter(name => name.endsWith('.yml'))) {
    for (const [id, job] of Object.entries(workflowJobs(file))) {
      if (/actions: write/.test(job.permissions)) writers.push(`${file}:${id}`);
    }
  }
  assert.deepEqual(writers, ['ci.yml:cache-cleanup']);
  assert.equal(plan('pull_request', ['Makefile'], harness)['cache-cleanup'], false);
});

// The whole-tree gates run on every run of their job: a change selects the
// job, and the gate's finding need not be in the change.
const wholeTreeGates = { frontend: ['make frontend-deadcode'], 'go-lint': ['make go-lint-whole-tree'] };

test('each whole-tree gate is a step of its job with no condition of its own', () => {
  const found = ciJobs();
  for (const [id, targets] of Object.entries(wholeTreeGates)) {
    for (const target of targets) {
      const steps = found[id].steps.filter(step => new RegExp(`^\\s*(- )?run: ${target}$`, 'm').test(step));
      assert.equal(steps.length, 1, `${id} runs ${target} once`);
      assert.doesNotMatch(steps[0], /^\s*(- )?if:/m, `${id} runs ${target} on a condition`);
    }
  }
});
