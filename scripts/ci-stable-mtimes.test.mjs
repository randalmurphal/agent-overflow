import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { lstatSync, mkdirSync, mkdtempSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { blobTime, directoryTime, stamp } from './ci-stable-mtimes.mjs';

const git = (dir, ...args) => execFileSync('git', ['-C', dir, ...args], {
  encoding: 'utf8',
  env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@example.invalid', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@example.invalid' },
});

function repo(t, files) {
  const dir = mkdtempSync(resolve(tmpdir(), 'ao-stable-mtimes-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  git(dir, 'init', '-q');
  for (const [path, body] of Object.entries(files)) {
    mkdirSync(join(dir, path, '..'), { recursive: true });
    writeFileSync(join(dir, path), body);
  }
  symlinkSync('AGENTS.md', join(dir, 'CLAUDE.md'));
  git(dir, 'add', '-A');
  git(dir, 'commit', '-qm', 'tree');
  return dir;
}

const ns = path => statSync(path, { bigint: true }).mtimeNs;
const linkNs = path => lstatSync(path, { bigint: true }).mtimeNs;
const seconds = path => Number(ns(path)) / 1e9;
const oid = (dir, path) => git(dir, 'rev-parse', `HEAD:${path}`).trim();

test('a tracked file gets its blob time, the same in every checkout', t => {
  const files = { 'AGENTS.md': 'guide\n', 'internal/x/x.go': 'package x\n' };
  const one = repo(t, files);
  const two = repo(t, files);
  stamp(one);
  stamp(two);
  for (const path of Object.keys(files)) {
    const time = seconds(join(one, path));
    assert.ok(Math.abs(time - blobTime(oid(one, path))) < 1e-5, path);
    assert.equal(ns(join(one, path)), ns(join(two, path)), path);
    assert.ok(time < Date.now() / 1000 - 2, `${path} is old enough for Go to cache`);
  }
  // The symlink itself carries the time of its target string's blob.
  assert.equal(linkNs(join(one, 'CLAUDE.md')), linkNs(join(two, 'CLAUDE.md')));
  assert.ok(Math.abs(Number(linkNs(join(one, 'CLAUDE.md'))) / 1e9 - blobTime(oid(one, 'CLAUDE.md'))) < 1e-5);
});

test('changed bytes of the same size get another time', t => {
  const one = repo(t, { 'data.json': '{"a":1}\n' });
  const two = repo(t, { 'data.json': '{"a":2}\n' });
  stamp(one);
  stamp(two);
  assert.notEqual(ns(join(one, 'data.json')), ns(join(two, 'data.json')));
});

test('directories get the fixed time, untracked files keep theirs', t => {
  const dir = repo(t, { 'internal/x/x.go': 'package x\n' });
  mkdirSync(join(dir, 'bin'));
  writeFileSync(join(dir, 'bin', 'tool'), 'binary');
  writeFileSync(join(dir, 'internal', 'scratch.txt'), 'untracked');
  const before = ns(join(dir, 'bin', 'tool'));
  const counts = stamp(dir);
  assert.deepEqual(counts, { files: 2, directories: 3, untracked: 2 });
  for (const path of ['.', 'internal', 'internal/x', 'bin', '.git']) {
    assert.equal(seconds(join(dir, path)), directoryTime, path);
  }
  assert.equal(ns(join(dir, 'bin', 'tool')), before);
  assert.ok(seconds(join(dir, 'internal', 'scratch.txt')) > Date.now() / 1000 - 60);
});

test('a malformed object id fails', () => {
  assert.throws(() => blobTime('not-an-id'), /not a git object id/);
});
