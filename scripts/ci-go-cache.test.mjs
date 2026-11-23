import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, rmSync, statSync, utimesSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { backdate, entries, prune } from './ci-go-cache.mjs';

function cache(t) {
  const dir = mkdtempSync(resolve(tmpdir(), 'ao-go-cache-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, 'README'), 'go cache');
  writeFileSync(join(dir, 'trim.txt'), '1');
  for (const shard of ['0a', 'ff']) mkdirSync(join(dir, shard));
  writeFileSync(join(dir, '0a', '0a11-a'), 'action');
  writeFileSync(join(dir, '0a', '0a11-d'), 'output bytes');
  writeFileSync(join(dir, 'ff', 'ff22-a'), 'action');
  // A linked executable is a directory entry.
  mkdirSync(join(dir, 'ff', 'ff22-d'));
  writeFileSync(join(dir, 'ff', 'ff22-d', 'tool'), 'binary');
  return dir;
}

// use refreshes an entry as Go does when it reads one over an hour old.
const use = path => utimesSync(path, Date.now() / 1000, Date.now() / 1000);

test('prune deletes the restored entries the run did not use', t => {
  const dir = cache(t);
  const state = backdate(dir);
  assert.equal(state.entries, 4);
  use(join(dir, '0a', '0a11-a'));
  use(join(dir, '0a', '0a11-d'));
  writeFileSync(join(dir, 'ff', 'ff33-a'), 'new this run');
  const result = prune(dir, state);
  assert.deepEqual({ kept: result.kept, deleted: result.deleted, keptAll: result.keptAll }, { kept: 3, deleted: 2, keptAll: false });
  assert.equal(result.deletedBytes, 'action'.length + 'binary'.length);
  assert.deepEqual(entries(dir).map(path => path.slice(dir.length + 1)).sort(), ['0a/0a11-a', '0a/0a11-d', 'ff/ff33-a']);
  assert.ok(existsSync(join(dir, 'README')) && existsSync(join(dir, 'trim.txt')));
});

test('prune keeps everything when no restored entry was refreshed', t => {
  const dir = cache(t);
  const state = backdate(dir);
  writeFileSync(join(dir, 'ff', 'ff33-a'), 'new this run');
  const result = prune(dir, state);
  assert.equal(result.keptAll, true);
  assert.equal(result.kept, 5);
  assert.equal(entries(dir).length, 5);
});

test('an empty restore keeps what the run built', t => {
  const dir = mkdtempSync(resolve(tmpdir(), 'ao-go-cache-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const state = backdate(dir);
  mkdirSync(join(dir, '1b'));
  writeFileSync(join(dir, '1b', '1b44-d'), 'built');
  assert.deepEqual(prune(dir, state), { kept: 1, keptBytes: 5, deleted: 0, deletedBytes: 0, keptAll: false });
});

test('backdate sets entries past the hour after which Go refreshes one it uses', t => {
  const dir = cache(t);
  const now = Date.now() / 1000;
  const state = backdate(dir, now);
  for (const path of entries(dir)) {
    const age = now - statSync(path).mtimeMs / 1000;
    assert.ok(age > 60 * 60 && age < 5 * 24 * 60 * 60, `${path} is ${age}s old`);
  }
  assert.ok(state.time < now - 60 * 60);
});

test('an output that a fresh index record names stays, though Go did not touch it', t => {
  const dir = mkdtempSync(resolve(tmpdir(), 'ao-go-cache-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const action = '1a'.padEnd(64, '1');
  const output = '2b'.padEnd(64, '2');
  const stale = '3c'.padEnd(64, '3');
  for (const shard of ['1a', '2b', '3c']) mkdirSync(join(dir, shard));
  const record = join(dir, '1a', `${action}-a`);
  writeFileSync(record, `v1 ${action} ${'0'.repeat(64)} 5 1\n`);
  writeFileSync(join(dir, '2b', `${output}-d`), 'bytes');
  writeFileSync(join(dir, '3c', `${stale}-d`), 'stale');
  const state = backdate(dir);
  // The run rewrote the index record to name the output it already had.
  writeFileSync(record, `v1 ${action} ${output} 5 2\n`);
  const result = prune(dir, state);
  assert.deepEqual({ kept: result.kept, deleted: result.deleted }, { kept: 2, deleted: 1 });
  assert.ok(existsSync(join(dir, '2b', `${output}-d`)));
  assert.ok(!existsSync(join(dir, '3c', `${stale}-d`)));
});
