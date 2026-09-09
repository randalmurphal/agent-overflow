// The Android adoption smoke needs a genuinely newer release, not merely a
// different hash. Temporarily stamp only generated metadata while embedding
// the real SPA, then restore it. Never run concurrently with another build.
import { execFileSync } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { nextPatchVersion, stampBundle, BUNDLE_ID_FILE, BUNDLE_RELEASE_FILE } from '../../frontend/scripts/bundleId.ts';
import { compareBundleVersions } from '../../frontend/src/lib/native/bundleVersion.ts';

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
try {
  await stampBundle(dist, version);
  execFileSync('go', ['build', '-ldflags', `-X main.version=${source}`, '-o', path.join(repo, 'bin/ao-android-harness'), '.'], { cwd: repo, stdio: 'inherit' });
  console.log(`==> Android adoption fixture release ${version} (APK ${packaged})`);
} finally {
  await Promise.all(names.map((name, i) => writeFile(path.join(dist, name), originals[i])));
}
