import { test } from 'node:test';
import assert from 'node:assert/strict';
import { capBytes, plan } from './ci-cache-cleanup.mjs';

const sha = n => String(n).repeat(40).slice(0, 40);
let id = 0;
const entry = (key, created, size = 1e9, ref = 'refs/heads/main') => ({ id: ++id, key, ref, created_at: created, size_in_bytes: size });
const keys = list => list.map(cache => cache.key).sort();
const start = '2026-11-22T10:00:00Z';

test('a slot keeps its newest entry and those made since the run started', () => {
  const caches = [
    entry(`ao-ci-go-build-test-store-Linux-${sha(1)}`, '2026-11-20T10:00:00Z'),
    entry(`ao-ci-go-build-test-store-Linux-${sha(2)}`, '2026-11-21T10:00:00Z'),
    // A slot this run did not save keeps its one entry.
    entry(`ao-ci-go-build-lint-Linux-${sha(1)}`, '2026-11-20T10:00:00Z'),
    // This run's save, and a newer run's beside it, retire everything older.
    entry(`ao-ci-go-build-harness-Linux-${sha(1)}`, '2026-11-21T10:00:00Z'),
    entry(`ao-ci-go-build-harness-Linux-${sha(2)}`, '2026-11-22T10:05:00Z'),
    entry(`ao-ci-go-build-harness-Linux-${sha(3)}`, '2026-11-22T10:06:00Z'),
  ];
  const result = plan(caches, start);
  assert.deepEqual(keys(result.keep), [
    `ao-ci-go-build-harness-Linux-${sha(2)}`, `ao-ci-go-build-harness-Linux-${sha(3)}`,
    `ao-ci-go-build-lint-Linux-${sha(1)}`, `ao-ci-go-build-test-store-Linux-${sha(2)}`,
  ]);
  assert.deepEqual(keys(result.remove), [`ao-ci-go-build-harness-Linux-${sha(1)}`, `ao-ci-go-build-test-store-Linux-${sha(1)}`]);
});

test('legacy shapes are retired, other caches are counted and never touched', () => {
  const caches = [
    entry(`go-build-test-noremote-store-triage-Linux-${'a'.repeat(64)}-${sha(1)}`, '2026-11-01T10:00:00Z'),
    entry(`go-mod-Linux-${'b'.repeat(64)}`, '2026-11-01T10:00:00Z'),
    entry(`node-cache-Linux-x64-pnpm-${'c'.repeat(64)}`, '2026-11-01T10:00:00Z'),
    entry('playwright-Linux-chromium webkit-abc', '2026-11-01T10:00:00Z'),
    entry('setup-go-Linux-x64-ubuntu24-go-1.27.4-abc', '2026-11-01T10:00:00Z', 2e9, 'refs/heads/release'),
    entry(`ao-ci-go-mod-Linux-${sha(4)}`, '2026-11-21T10:00:00Z'),
  ];
  const result = plan(caches, start);
  assert.equal(result.remove.length, 4);
  assert.ok(result.remove.every(cache => !cache.key.startsWith('setup-go') && !cache.key.startsWith('ao-ci-')));
  assert.deepEqual(keys(result.other), ['setup-go-Linux-x64-ubuntu24-go-1.27.4-abc']);
  assert.deepEqual({ managed: result.managedBytes, other: result.otherBytes }, { managed: 1e9, other: 2e9 });
});

test('an entry of a slot the pipeline no longer names is retired and named', () => {
  const result = plan([entry(`ao-ci-go-build-test-noremote-rest-Linux-${sha(1)}`, '2026-11-21T10:00:00Z')], start);
  assert.deepEqual(keys(result.remove), [`ao-ci-go-build-test-noremote-rest-Linux-${sha(1)}`]);
  assert.match(result.warnings[0], /no longer names/);
});

test('a managed entry from another ref is deleted and named', () => {
  const result = plan([entry(`ao-ci-go-build-lint-Linux-${sha(1)}`, '2026-11-21T10:00:00Z', 1e9, 'refs/pull/7/merge')], start);
  assert.equal(result.remove.length, 1);
  assert.match(result.warnings[0], /refs\/pull\/7\/merge saved ao-ci-go-build-lint/);
});

test('two generations of the pipeline caches and the others must fit under the cap', () => {
  // The 11 entries this repository held on 2026-11-22: one generation of
  // the pipeline's slots is 9.05 GB of them, held twice while a push saves.
  const measured = [659.13, 1691.02, 803.98, 777.39, 4206.93, 215.76, 71.05, 12.99, 485.93, 119.57].map(mb => mb * 1e6)
    .reduce((sum, size) => sum + size, 0);
  assert.equal(capBytes, 10e9);
  assert.equal(plan([entry(`ao-ci-go-build-lint-Linux-${sha(1)}`, '2026-11-21T10:00:00Z', measured)], start).over, true);
  const half = entry(`ao-ci-go-build-lint-Linux-${sha(1)}`, '2026-11-21T10:00:00Z', 4e9);
  assert.equal(plan([half], start).over, false);
  // Others count once: 2 x 4 GB + 2.5 GB is over.
  assert.equal(plan([half, entry('setup-go-Linux-x64-abc', '2026-11-21T10:00:00Z', 2.5e9, 'refs/heads/release')], start).over, true);
});

test('a run start that is not a time fails', () => {
  assert.throws(() => plan([entry(`ao-ci-go-mod-Linux-${sha(1)}`, '2026-11-21T10:00:00Z')], 'soon'), /not a time/);
});
