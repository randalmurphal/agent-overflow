import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { harnessTestPackages, jobs, plan, suites, verdict } from './ci-suites.mjs';

const harness = harnessTestPackages('# packages\nE2E_TEST_PACKAGES := ./internal/frontendclient ./internal/store\n');
const run = paths => suites(paths, harness);
const none = { 'go-checks': false, 'go-lint': false, frontend: false, vitest: false, e2e: false, 'upgrade-lineage': false };

test('the run declaration names the harness test packages', () => {
  assert.deepEqual(harness, ['internal/frontendclient', 'internal/store']);
  assert.ok(harnessTestPackages().length > 0);
  assert.throws(() => harnessTestPackages('E2E_COMMANDS := x\n'), /no longer sets E2E_TEST_PACKAGES/);
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
    'docs/references/fixtures/claude/cron_create_fire_20261010.ndjson', 'internal/workflow/starters/content/build-and-validate/implement.md',
    'scripts/perfprobe/probe', '.github/workflows/ci.yml', 'ci/Dockerfile', 'mobile/package.json', 'build/config.yml']) {
    const result = run(['docs/x.md', path]);
    assert.deepEqual({ ...result, 'upgrade-lineage': false },
      { 'go-checks': true, 'go-lint': true, frontend: true, vitest: true, e2e: true, 'upgrade-lineage': false }, path);
  }
});

test('the upgrade lineage check runs for the store, its corpus, the check and the version', () => {
  for (const path of ['internal/store/migrations.go', 'e2e/tests/upgrade-corpus-config.ts', 'scripts/release-upgrade-check.sh',
    'build/config.yml', 'frontend/package.json']) {
    assert.equal(run([path])['upgrade-lineage'], true, path);
  }
  for (const path of ['internal/storex/x.go', 'e2e/tests/pane-find.spec.ts', 'frontend/package-lock.json']) {
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

// ciJobs reads ci.yml's jobs: each one's id, needs and if.
function ciJobs() {
  const text = readFileSync(new URL('../.github/workflows/ci.yml', import.meta.url), 'utf8');
  const body = text.slice(text.indexOf('\njobs:\n'));
  const found = {};
  for (const block of body.split(/\n(?=  [a-z0-9-]+:\n)/).slice(1)) {
    const id = /^  ([a-z0-9-]+):/.exec(block)[1];
    const needs = /\n    needs: (\[[^\]]*\]|[a-z0-9-]+)\n/.exec(block)?.[1] ?? '';
    found[id] = { needs: needs.replace(/[[\]\s]/g, '').split(',').filter(Boolean), if: /\n    if: (.*)\n/.exec(block)?.[1] ?? '' };
  }
  return found;
}

test('every job ci.yml runs follows its own key of the plan, and the result job needs them all', () => {
  const found = ciJobs();
  const decided = Object.keys(found).filter(id => id !== 'toolchain' && id !== 'ci-result').sort();
  assert.deepEqual(decided, Object.keys(jobs).sort());
  for (const id of decided) {
    assert.ok(found[id].needs.includes('toolchain'), `${id} needs toolchain`);
    assert.ok(found[id].if.includes(`fromJSON(needs.toolchain.outputs.plan || '{}')['${id}']`), `${id}: if: ${found[id].if}`);
  }
  assert.deepEqual(found['ci-result'].needs.sort(), ['toolchain', ...decided].sort());
});
