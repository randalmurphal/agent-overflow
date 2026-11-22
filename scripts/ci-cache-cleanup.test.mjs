import { test } from 'node:test';
import assert from 'node:assert/strict';
import { plan } from './ci-cache-cleanup.mjs';

const sha = n => String(n).repeat(40).slice(0, 40);
// main lists its newest commit first.
const main = [sha(3), sha(2), sha(1)];
let id = 0;
const entry = (key, created, size = 1e9, ref = 'refs/heads/main') => ({ id: ++id, key, ref, created_at: created, size_in_bytes: size });
const keys = list => list.map(cache => cache.key).sort();

test('the newest commit on main stays, older commits of the slot go', () => {
  const caches = [
    entry(`go-build-test-store-Linux-${sha(1)}`, '2026-11-20T10:00:00Z'),
    entry(`go-build-test-store-Linux-${sha(2)}`, '2026-11-21T10:00:00Z'),
    entry(`go-build-lint-Linux-${sha(1)}`, '2026-11-20T10:00:00Z'),
  ];
  const result = plan(caches, main, '2026-11-22T10:00:00Z');
  assert.deepEqual(keys(result.keep), [`go-build-lint-Linux-${sha(1)}`, `go-build-test-store-Linux-${sha(2)}`]);
  assert.deepEqual(keys(result.remove), [`go-build-test-store-Linux-${sha(1)}`]);
  assert.equal(result.total, 2e9);
  assert.equal(result.over, false);
});

test('an older commit saved after a newer one still loses to it, unless this run saved it', () => {
  // The run for sha(2) finished after the run for sha(3), so its save is the
  // later one; the commit order decides, and the run start protects saves
  // made during this run.
  const caches = [
    entry(`go-build-harness-Linux-${sha(3)}`, '2026-11-22T10:05:00Z'),
    entry(`go-build-harness-Linux-${sha(2)}`, '2026-11-22T10:09:00Z'),
  ];
  assert.deepEqual(keys(plan(caches, main, '2026-11-22T11:00:00Z').remove), [`go-build-harness-Linux-${sha(2)}`]);
  assert.deepEqual(plan(caches, main, '2026-11-22T10:07:00Z').remove, []);
});

test('a commit main no longer lists is older than every listed one', () => {
  const caches = [
    entry(`go-build-checks-Linux-${sha(9)}`, '2026-11-22T10:00:00Z'),
    entry(`go-build-checks-Linux-${sha(1)}`, '2026-11-01T10:00:00Z'),
  ];
  assert.deepEqual(keys(plan(caches, main, '2026-11-23T00:00:00Z').remove), [`go-build-checks-Linux-${sha(9)}`]);
});

test('a lockfile-keyed cache keeps its newest entry', () => {
  const caches = [
    entry('pnpm-store-macOS-aaaa', '2026-11-20T10:00:00Z', 2e8),
    entry('pnpm-store-macOS-bbbb', '2026-11-21T10:00:00Z', 2e8),
  ];
  assert.deepEqual(keys(plan(caches, main, '2026-11-22T00:00:00Z').remove), ['pnpm-store-macOS-aaaa']);
});

test('a cache another ref saved is deleted and named', () => {
  const caches = [entry('go-mod-Linux-cccc', '2026-11-21T10:00:00Z', 3e8, 'refs/pull/299/merge')];
  const result = plan(caches, main, '2026-11-22T00:00:00Z');
  assert.deepEqual(keys(result.remove), ['go-mod-Linux-cccc']);
  assert.match(result.warnings[0], /refs\/pull\/299\/merge saved go-mod-Linux-cccc/);
  assert.equal(result.total, 0);
});

test('entries over the budget fail the cleanup', () => {
  const caches = [entry(`go-build-lint-Linux-${sha(3)}`, '2026-11-22T10:00:00Z', 5e9), entry(`go-build-test-rest-Linux-${sha(3)}`, '2026-11-22T10:00:00Z', 4e9)];
  assert.equal(plan(caches, main, '2026-11-22T00:00:00Z').over, true);
  assert.throws(() => plan([], main, 'yesterday'), /not a time/);
});
