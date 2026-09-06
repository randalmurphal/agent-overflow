// Promote immutable GitHub Actions artifacts; never build or publish from here.
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { createReadStream, readdirSync, lstatSync, readFileSync, writeFileSync, openSync, closeSync, mkdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const assets = [
  'agent-overflow-linux-amd64', 'agent-overflow-headless-linux-amd64',
  'agent-overflow-wsl-amd64.exe', 'agent-overflow-darwin-arm64.zip',
  'agent-overflow-android.apk', 'install.sh', 'appicon.png',
];
const manifestName = 'CANDIDATE.json';
const files = [...assets, manifestName, 'SHASUMS256'].sort();
const fail = message => { throw new Error(message); };
const gh = args => execFileSync('gh', args, { encoding: 'utf8' });
const api = path => JSON.parse(gh(['api', path]));
const digest = async path => {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
};

export function identity(env) {
  const { GITHUB_REPOSITORY: repository, GITHUB_SHA: commit, VERSION: version } = env;
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '') || !/^[a-f0-9]{40}$/.test(commit ?? '') ||
      !/^[0-9][\w.+-]*$/.test(version ?? '') || version.includes('..')) fail('Invalid candidate repository, commit or version.');
  return { repository, commit, version };
}

export function selectCandidate(runs, repository, commit) {
  const matches = runs.filter(run => run.event === 'workflow_dispatch' && run.status === 'completed' &&
    run.conclusion === 'success' && run.head_sha === commit && run.repository?.full_name === repository);
  if (matches.length !== 1) fail(matches.length ?
    `Multiple saved candidates exist: ${matches.map(run => run.id).join(', ')}. Retain only the tested candidate artifact and retry; nothing is deleted automatically.` :
    'No successful manual release candidate for this exact commit. Build and test it before tagging.');
  return matches[0];
}

export async function verify(dir, expected) {
  if (JSON.stringify(readdirSync(dir).sort()) !== JSON.stringify(files)) fail('Candidate has missing or unexpected files.');
  if (files.some(name => !lstatSync(resolve(dir, name)).isFile())) fail('Candidate entries must be regular files.');
  const manifest = JSON.parse(readFileSync(resolve(dir, manifestName), 'utf8'));
  for (const [key, value] of Object.entries(expected)) {
    if (manifest[key] !== value) fail(`Candidate ${key} does not match the selected workflow run.`);
  }
  const checks = readFileSync(resolve(dir, 'SHASUMS256'), 'utf8').trim().split('\n');
  const seen = new Set();
  for (const line of checks) {
    const match = /^([a-f0-9]{64})  \.\/([\w.-]+)$/.exec(line);
    if (!match || !files.includes(match[2]) || match[2] === 'SHASUMS256' || seen.has(match[2])) fail('Invalid candidate checksum manifest.');
    seen.add(match[2]);
    if (await digest(resolve(dir, match[2])) !== match[1]) fail(`Candidate checksum mismatch: ${match[2]}`);
  }
  if (seen.size !== files.length - 1) fail('Candidate checksum manifest is incomplete.');
}

async function main() {
  const [command, dir] = process.argv.slice(2);
  const expected = identity(process.env);
  if (!dir) fail('Usage: release-candidate.mjs stamp|download DIRECTORY');
  if (command === 'stamp') {
    const run = Number(process.env.GITHUB_RUN_ID), attempt = Number(process.env.GITHUB_RUN_ATTEMPT);
    if (!Number.isSafeInteger(run) || run <= 0 || !Number.isSafeInteger(attempt) || attempt <= 0) fail('Invalid candidate workflow run.');
    writeFileSync(resolve(dir, manifestName), JSON.stringify({ ...expected, run, attempt }, null, 2) + '\n');
    return;
  }
  if (command !== 'download') fail('Unknown candidate command.');
  const base = `repos/${expected.repository}/actions`;
  const pages = JSON.parse(gh(['api', '--paginate', '--slurp',
    `${base}/workflows/release-build.yml/runs?event=workflow_dispatch&status=success&head_sha=${expected.commit}&per_page=100`]));
  const eligible = [];
  for (const run of pages.flatMap(page => page.workflow_runs)) {
    if (run.event !== 'workflow_dispatch' || run.status !== 'completed' || run.conclusion !== 'success' ||
        run.head_sha !== expected.commit || run.repository?.full_name !== expected.repository) continue;
    const artifacts = JSON.parse(gh(['api', '--paginate', '--slurp', `${base}/runs/${run.id}/artifacts?per_page=100`]))
      .flatMap(page => page.artifacts).filter(a => a.name === `agent-overflow-release-${expected.version}` && !a.expired);
    for (const artifact of artifacts) eligible.push({ ...run, artifact });
  }
  if (!eligible.length) fail('Saved candidate is missing or expired. Build and test a manual candidate for this exact commit/version; promotion never rebuilds.');
  const run = selectCandidate(eligible, expected.repository, expected.commit);
  const artifact = run.artifact;
  if (!/^sha256:[a-f0-9]{64}$/.test(artifact.digest ?? '')) fail('GitHub did not provide a SHA-256 digest for this candidate.');
  const archive = `${resolve(dir)}.zip`;
  const fd = openSync(archive, 'wx');
  try { execFileSync('gh', ['api', `${base}/artifacts/${artifact.id}/zip`], { stdio: ['ignore', fd, 'inherit'] }); }
  finally { closeSync(fd); }
  if (`sha256:${await digest(archive)}` !== artifact.digest) fail('Downloaded artifact does not match GitHub’s immutable artifact digest.');
  const entries = execFileSync('unzip', ['-Z1', archive], { encoding: 'utf8' }).trim().split('\n').sort();
  if (JSON.stringify(entries) !== JSON.stringify(files)) fail('Candidate archive has missing, duplicate or unexpected paths.');
  mkdirSync(dir); // Refuse to merge with any previous download.
  execFileSync('unzip', ['-q', archive, '-d', dir]);
  await verify(dir, { ...expected, run: run.id, attempt: run.run_attempt });
  // A rerun can supersede the selected attempt while we download. Fail closed.
  const current = api(`${base}/runs/${run.id}`);
  if (current.run_attempt !== run.run_attempt || current.status !== 'completed' || current.conclusion !== 'success') fail('Candidate run changed during promotion.');
  console.log(`Verified candidate from ${run.html_url} (attempt ${run.run_attempt}); publish these bytes unchanged.`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => { console.error(error.message); process.exitCode = 1; });
}
