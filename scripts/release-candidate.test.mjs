import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, rmSync, mkdirSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve, delimiter } from 'node:path';
import { fileURLToPath } from 'node:url';
import { identity, selectCandidate, verify } from './release-candidate.mjs';

const expected = { repository: 'owner/repo', commit: 'a'.repeat(40), version: '0.0.15', run: 42, attempt: 1 };
const run = { id: 42, run_attempt: 1, event: 'workflow_dispatch', status: 'completed', conclusion: 'success',
  head_sha: expected.commit, repository: { full_name: expected.repository }, html_url: 'https://example.invalid/run/42' };
const hash = data => createHash('sha256').update(data).digest('hex');
const names = ['agent-overflow-linux-amd64', 'agent-overflow-headless-linux-amd64', 'agent-overflow-wsl-amd64.exe',
  'agent-overflow-darwin-arm64.zip', 'agent-overflow-android.apk', 'install.sh', 'appicon.png', 'CANDIDATE.json'];
function fixture(t) {
  const dir = mkdtempSync(resolve(tmpdir(), 'ao-candidate-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  for (const name of names) writeFileSync(resolve(dir, name), name === 'CANDIDATE.json' ? JSON.stringify(expected) : `tested bytes: ${name}`);
  writeFileSync(resolve(dir, 'SHASUMS256'), names.map(name => `${hash(readFileSync(resolve(dir, name)))}  ./${name}\n`).join(''));
  return dir;
}

test('only one successful exact-commit manual run is eligible', () => {
  for (const changed of [{ event: 'push' }, { conclusion: 'failure' }, { status: 'in_progress' },
    { head_sha: 'b'.repeat(40) }, { repository: { full_name: 'other/repo' } }]) {
    assert.throws(() => selectCandidate([{ ...run, ...changed }], expected.repository, expected.commit));
  }
  assert.equal(selectCandidate([run], expected.repository, expected.commit), run);
  assert.throws(() => selectCandidate([run, { ...run, id: 43 }], expected.repository, expected.commit), /42, 43.*Retain/);
  assert.throws(() => identity({ GITHUB_REPOSITORY: 'owner/repo', GITHUB_SHA: expected.commit, VERSION: '../escape' }));
});

test('candidate manifest, complete file set and every byte must match', async t => {
  const dir = fixture(t);
  await verify(dir, expected);
  for (const changed of [{ version: '0.0.16' }, { commit: 'b'.repeat(40) }, { run: 43 }, { attempt: 2 }]) {
    await assert.rejects(verify(dir, { ...expected, ...changed }), /does not match/);
  }
  writeFileSync(resolve(dir, 'unexpected'), 'extra');
  await assert.rejects(verify(dir, expected), /unexpected/);
  rmSync(resolve(dir, 'unexpected'));
  const checksums = readFileSync(resolve(dir, 'SHASUMS256'), 'utf8');
  writeFileSync(resolve(dir, 'SHASUMS256'), checksums + checksums.split('\n')[0] + '\n');
  await assert.rejects(verify(dir, expected), /checksum manifest/);
  writeFileSync(resolve(dir, 'SHASUMS256'), checksums);
  rmSync(resolve(dir, names[0]));
  symlinkSync(resolve(dir, names[1]), resolve(dir, names[0]));
  await assert.rejects(verify(dir, expected), /regular files/);
  rmSync(resolve(dir, names[0]));
  writeFileSync(resolve(dir, names[0]), 'changed bytes');
  await assert.rejects(verify(dir, expected), /checksum mismatch/);
});

test('download promotes immutable bytes; rejects digest corruption, expired, ambiguous and rerun candidates', t => {
  const source = fixture(t);
  const work = mkdtempSync(resolve(tmpdir(), 'ao-promotion-'));
  t.after(() => rmSync(work, { recursive: true, force: true }));
  const archive = resolve(work, 'source.zip');
  execFileSync('zip', ['-q', archive, ...names, 'SHASUMS256'], { cwd: source });
  const artifact = { id: 7, name: `agent-overflow-release-${expected.version}`, expired: false, digest: `sha256:${hash(readFileSync(archive))}` };
  const gh = resolve(work, 'gh');
  writeFileSync(gh, `#!/usr/bin/env node
const fs = require('node:fs');
const data=JSON.parse(fs.readFileSync(process.env.CANDIDATE_FIXTURE));
const endpoint=process.argv.at(-1);
if(endpoint.endsWith('/zip')) process.stdout.write(fs.readFileSync(data.archive));
else if(endpoint.includes('/workflows/')) console.log(JSON.stringify([{workflow_runs:data.runs}]));
else if(endpoint.includes('/artifacts?')) console.log(JSON.stringify([{artifacts:data.artifacts}]));
else console.log(JSON.stringify(data.current));
`, { mode: 0o755 });
  const config = resolve(work, 'fixture.json');
  const script = fileURLToPath(new URL('./release-candidate.mjs', import.meta.url));
  const invoke = (name, changed = {}) => {
    writeFileSync(config, JSON.stringify({ archive, runs: [run], artifacts: [artifact], current: run, ...changed }));
    return spawnSync(process.execPath, [script, 'download', resolve(work, name)], { encoding: 'utf8', env: {
      ...process.env, PATH: work + delimiter + process.env.PATH, CANDIDATE_FIXTURE: config,
      GITHUB_REPOSITORY: expected.repository, GITHUB_SHA: expected.commit, VERSION: expected.version,
    } });
  };
  const success = invoke('good');
  assert.equal(success.status, 0, success.stderr);
  for (const name of [...names, 'SHASUMS256']) assert.deepEqual(readFileSync(resolve(work, 'good', name)), readFileSync(resolve(source, name)));
  assert.match(invoke('bad-digest', { artifacts: [{ ...artifact, digest: `sha256:${'0'.repeat(64)}` }] }).stderr, /immutable artifact digest/);
  assert.match(invoke('expired', { artifacts: [{ ...artifact, expired: true }] }).stderr, /missing or expired/);
  assert.match(invoke('missing', { artifacts: [] }).stderr, /missing or expired/);
  assert.match(invoke('ambiguous', { runs: [run, { ...run, id: 43 }] }).stderr, /42, 43/);
  assert.match(invoke('rerun', { current: { ...run, run_attempt: 2 } }).stderr, /changed during promotion/);
  mkdirSync(resolve(work, 'already-there'));
  assert.notEqual(invoke('already-there').status, 0);
});
