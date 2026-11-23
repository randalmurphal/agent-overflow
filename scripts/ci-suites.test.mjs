import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, readdirSync } from 'node:fs';
import { harnessTestPackages, jobs, plan, suites, verdict } from './ci-suites.mjs';

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
    [['e2e/playwright.config.ts'], { ...none, e2e: true }],
    [['e2e/package.json'], { ...none, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'e2e/src/harness.ts'], { ...none, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'docs/specs/x.md'], { ...none, e2e: true }],
    [['frontend/src/lib/components/chat/AGENTS.md', 'internal/app/CLAUDE.md'], none],
    [['e2e/tests/pane-find.spec.ts', 'e2e/scripts/android-smoke.sh'], { ...none, e2e: true }],
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
    [['e2e/tests/pane-find.spec.ts', 'internal/app/app.go'], { ...none, 'go-checks': true, 'go-lint': true, vitest: true, e2e: true }],
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
  assert.equal(plan('workflow_dispatch', undefined, harness)['cache-cleanup'], false);
});
