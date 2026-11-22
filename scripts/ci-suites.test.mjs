import { test } from 'node:test';
import assert from 'node:assert/strict';
import { harnessTestPackages, suites } from './ci-suites.mjs';

const harness = harnessTestPackages();
const run = paths => suites(paths, harness);
const none = { 'go-checks': false, 'go-lint': false, frontend: false, vitest: false, e2e: false, 'upgrade-lineage': false };

test('the Makefile names the harness test packages', () => {
  assert.deepEqual(harness, ['internal/frontendclient', 'internal/store']);
  assert.throws(() => harnessTestPackages('all: x\n'), /no longer sets HARNESS_TEST_PKGS/);
  assert.throws(() => harnessTestPackages('HARNESS_TEST_PKGS := ./internal/...\n'), /no plain package directories/);
});

test('each change runs the suites that read it', () => {
  const cases = [
    [['docs/architecture/development.md', 'internal/app/AGENTS.md', 'frontend/CLAUDE.md', 'README.md'], none],
    [['e2e/playwright.config.ts'], { ...none, e2e: true }],
    [['e2e/package.json'], { ...none, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'e2e/src/harness.ts'], { ...none, e2e: true }],
    [['e2e/tests/pane-find.spec.ts', 'docs/specs/x.md'], { ...none, e2e: true }],
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
