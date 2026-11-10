// The Android adoption smoke needs a genuinely newer release, not merely a
// different hash. Temporarily stamp only generated metadata while embedding
// the real SPA, then restore it. Never run concurrently with another build.
// A signal stops the compiler and waits for it before the restore, then
// exits as that signal would.
import { execFileSync, spawn } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { constants } from 'node:os';
import { setTimeout as sleep } from 'node:timers/promises';
import path from 'node:path';
import { nextPatchVersion, stampBundle, BUNDLE_ID_FILE, BUNDLE_RELEASE_FILE } from '../../frontend/scripts/bundleId.ts';
import { compareBundleVersions } from '../../frontend/src/lib/native/bundleVersion.ts';

// How long a stopped build's processes get after SIGTERM, then SIGKILL.
const STOP_GRACE_MS = 10_000;
const repo = path.resolve(import.meta.dirname, '../..');
const apk = process.argv[2];
if (!apk) throw new Error('The Android fixture requires the APK being tested.');
const packaged = JSON.parse(execFileSync('unzip', ['-p', apk, `assets/public/${BUNDLE_RELEASE_FILE}`], { encoding: 'utf8' })).version;
const dist = path.join(repo, 'frontend/dist');
const source = JSON.parse(await readFile(path.join(dist, BUNDLE_RELEASE_FILE), 'utf8')).version;
if (typeof source !== 'string' || typeof packaged !== 'string') throw new Error('Both bundles require release metadata.');
const order = compareBundleVersions(source, packaged);
if (order === null) throw new Error('Both bundles require valid SemVer releases.');
const version = nextPatchVersion(order >= 0 ? source : packaged);
const names = [BUNDLE_RELEASE_FILE, BUNDLE_ID_FILE];
const originals = await Promise.all(names.map((name) => readFile(path.join(dist, name))));
let stopping: NodeJS.Signals | undefined;
let stopCompiler = () => {};
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP'] as const) {
  process.on(signal, () => {
    stopping ??= signal;
    stopCompiler();
  });
}
try {
  await stampBundle(dist, version);
  if (!stopping) {
    // Its own process group, so the stop reaches the compilers it runs.
    const compiler = spawn('go', ['build', '-ldflags', `-X main.version=${source}`, '-o', path.join(repo, 'bin/ao-android-harness'), '.'], { cwd: repo, stdio: 'inherit', detached: true });
    const exited = new Promise<number | null>((resolve, reject) => {
      compiler.once('error', reject);
      compiler.once('exit', (code) => resolve(code));
    });
    const group = compiler.pid;
    if (group !== undefined) {
      stopCompiler = () => {
        try {
          signalGroup(group, 'SIGTERM');
        } catch (error) {
          console.error(`stop go build: ${error}`);
        }
      };
    }
    const code = await exited;
    // A stopped build's compile steps can outlive go itself.
    if (stopping && group !== undefined) await groupGone(group);
    if (!stopping && code !== 0) throw new Error(`go build exited with ${code}`);
  }
  if (!stopping) console.log(`==> Android adoption fixture release ${version} (APK ${packaged})`);
} finally {
  await Promise.all(names.map((name, i) => writeFile(path.join(dist, name), originals[i])));
}
if (stopping) process.exit(128 + constants.signals[stopping]);

// signalGroup sends signal to a process group and says whether any member
// was left to receive it.
function signalGroup(group: number, signal: NodeJS.Signals | 0): boolean {
  try {
    process.kill(-group, signal);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ESRCH') return false;
    throw error;
  }
}

// groupGone waits for every member of a stopped group to exit, killing what
// outlives the grace.
async function groupGone(group: number): Promise<void> {
  const killAt = Date.now() + STOP_GRACE_MS;
  while (signalGroup(group, 0)) {
    const now = Date.now();
    if (now > killAt + STOP_GRACE_MS) throw new Error(`go build's process group ${group} survived SIGKILL`);
    if (now > killAt) signalGroup(group, 'SIGKILL');
    await sleep(50);
  }
}
