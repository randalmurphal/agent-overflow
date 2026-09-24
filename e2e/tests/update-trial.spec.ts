// Update trial and the no-live-migration gate against the real App boot
// (docs/specs/app-update.md, rule 7). The database is the v118 fixture the
// store's own test writes (TestHistoryRepairHarnessFixture, compiled into
// bin/ao-store-test by `make harness-build`); bin/agent-overflow runs the
// update commands and the headless boot as the Windows launcher runs them
// through wsl.exe. Covers: a headless boot started with
// --refuse-pending-migrations answers /bootstrap.json with the 409
// migrations-pending refusal and leaves the database files as they were;
// __update-snapshot reports the schema version the refusal named, which keys
// the launcher's failure memory; __update-snapshot and __update-trial-run
// migrate it in the real App trial
// boot, which reports prepared; the next gated boot finds nothing pending and
// becomes ready; and __update-restore after a prepared trial puts the
// fixture's bytes back. Every process runs with a temporary home and fake
// claude, codex, gh and glab first on PATH, which record each call: the
// refused boot and the parked trial start no provider, and the live boot's
// provider probes reach the fakes.
import { execFile, type ChildProcess } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { access, chmod, copyFile, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { createInterface } from 'node:readline';
import { promisify } from 'node:util';
import { spawnContained } from '../src/harness-process.js';
import { FALLBACK_MEMORY_LIMIT_BYTES } from '../src/harness-watchdog.js';
import { test, expect } from './fixtures.js';

const run = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, '..', '..');
const backend = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
const UPDATE_PREFIX = '__AO_UPDATE__:';
const BOOTSTRAP_PREFIX = '__AO_BOOTSTRAP__:';
const FIXTURE_VERSION = 118;

// Siblings of the backend binary, as history-repair-upgrade resolves them.
function sibling(name: string): string {
  return path.join(path.dirname(backend), name);
}

// The version `make harness-build` stamps into bin/agent-overflow.
async function buildVersion(): Promise<string> {
  const config = await readFile(path.join(repoRoot, 'build', 'config.yml'), 'utf8');
  const match = /^ {2}version: "(.+)"$/m.exec(config);
  if (!match) throw new Error('build/config.yml names no version');
  return match[1];
}

interface Sandbox {
  root: string;
  dataDir: string;
  dbPath: string;
  env: NodeJS.ProcessEnv;
  calls: string;
}

// A data root, a home and fake provider and forge CLIs. The environment is
// built from nothing, so no provider home override, WSL interop variable or
// user PATH entry reaches the backend; the login-shell PATH probe runs
// /bin/sh against the empty home.
async function sandbox(): Promise<Sandbox> {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-update-trial-'));
  const home = path.join(root, 'home');
  const bin = path.join(root, 'bin');
  const dataDir = path.join(root, 'data');
  const calls = path.join(root, 'cli-calls.log');
  await mkdir(home, { recursive: true });
  await mkdir(bin, { recursive: true });
  await mkdir(path.join(dataDir, 'agent-overflow'), { recursive: true });
  const versions: Record<string, string> = {
    claude: '2.1.200 (Claude Code)',
    codex: 'codex-cli 9.0.0',
    gh: 'gh version 2.0.0',
    glab: 'glab 1.0.0',
  };
  for (const [name, version] of Object.entries(versions)) {
    const script = path.join(bin, name);
    await writeFile(script, [
      '#!/bin/sh',
      `printf '%s\\n' "${name} $*" >> '${calls}'`,
      `if [ "$1" = --version ]; then echo '${version}'; exit 0; fi`,
      'exit 1',
      '',
    ].join('\n'));
    await chmod(script, 0o755);
  }
  const env: NodeJS.ProcessEnv = {
    HOME: home,
    USERPROFILE: home,
    XDG_CONFIG_HOME: home,
    XDG_DATA_HOME: home,
    XDG_CACHE_HOME: home,
    APPDATA: home,
    LOCALAPPDATA: home,
    SHELL: '/bin/sh',
    PATH: `${bin}:/usr/bin:/bin`,
    LANG: 'C.UTF-8',
    TMPDIR: path.join(root, 'tmp'),
  };
  await mkdir(env.TMPDIR!, { recursive: true });
  return { root, dataDir, dbPath: path.join(dataDir, 'agent-overflow', 'agent-overflow.db'), env, calls };
}

async function writeFixture(dbPath: string): Promise<void> {
  await run(sibling('ao-store-test'), ['-test.run', '^TestHistoryRepairHarnessFixture$', '-test.count=1'], {
    env: { ...process.env, AO_TEST_HISTORY_REPAIR_FIXTURE: dbPath },
    timeout: 120_000,
  });
  // The test skips, and exits 0, when it is not handed a path.
  await access(dbPath);
}

// The digest of each file of the database triple that exists.
async function databaseFiles(dbPath: string): Promise<Record<string, string>> {
  const dir = path.dirname(dbPath);
  const base = path.basename(dbPath);
  const out: Record<string, string> = {};
  for (const name of (await readdir(dir)).sort()) {
    if (name !== base && name !== `${base}-wal` && name !== `${base}-shm`) continue;
    out[name] = createHash('sha256').update(await readFile(path.join(dir, name))).digest('hex');
  }
  return out;
}

// The highest applied migration, read from a copy so the query cannot
// change the files the test compares.
async function schemaVersion(sb: Sandbox): Promise<number> {
  const copy = await mkdtemp(path.join(sb.root, 'query-'));
  const dir = path.dirname(sb.dbPath);
  for (const name of await readdir(dir)) {
    if (name === 'agent-overflow.db' || name === 'agent-overflow.db-wal') {
      await copyFile(path.join(dir, name), path.join(copy, name));
    }
  }
  const { stdout } = await run(sibling('ao-harness'), ['-o', 'json', 'db', '--file', path.join(copy, 'agent-overflow.db'),
    'SELECT MAX(version) AS v FROM migration_versions'], { timeout: 30_000 });
  await rm(copy, { recursive: true, force: true });
  return (JSON.parse(stdout) as Array<{ v: number }>)[0].v;
}

interface UpdateEvent {
  type: 'started' | 'progress' | 'result';
  outcome?: string;
  reason?: string;
  schema?: number;
  progress?: { phase: string; detail: string };
}

// runUpdateCommand runs one update command to its end, as
// wsllauncher.UpdateCommandRunner does, and returns its reports.
async function runUpdateCommand(sb: Sandbox, args: string[], timeoutMs = 120_000): Promise<{ events: UpdateEvent[]; stderr: string }> {
  const child = spawnContained(backend, [...args, '--data-dir', sb.dataDir], {
    env: sb.env,
    stdio: ['ignore', 'pipe', 'pipe'],
    memoryLimitBytes: FALLBACK_MEMORY_LIMIT_BYTES,
    detached: true,
  });
  const events: UpdateEvent[] = [];
  let stderr = '';
  createInterface({ input: child.stdout! }).on('line', (line) => {
    const at = line.indexOf(UPDATE_PREFIX);
    if (at >= 0) events.push(JSON.parse(line.slice(at + UPDATE_PREFIX.length)) as UpdateEvent);
  });
  child.stderr!.on('data', (chunk: Buffer) => {
    stderr = (stderr + chunk.toString()).slice(-64 * 1024);
  });
  const code = await exited(child, timeoutMs);
  if (code === undefined) throw new Error(`${args[0]} did not finish in ${timeoutMs}ms\n${stderr}`);
  return { events, stderr };
}

function result(events: UpdateEvent[]): UpdateEvent | undefined {
  return events.filter((event) => event.type === 'result').at(-1);
}

// exited waits for the child, and kills its process group after timeoutMs.
// It resolves the exit code, or undefined when the child had to be killed.
function exited(child: ChildProcess, timeoutMs: number): Promise<number | undefined> {
  return new Promise((resolve, reject) => {
    if (child.exitCode !== null) {
      resolve(child.exitCode);
      return;
    }
    const timer = setTimeout(() => {
      killGroup(child);
      child.once('close', () => resolve(undefined));
    }, timeoutMs);
    child.once('error', (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once('close', (code) => {
      clearTimeout(timer);
      resolve(code ?? -1);
    });
  });
}

function killGroup(child: ChildProcess): void {
  if (child.pid === undefined) return;
  try {
    process.kill(-child.pid, 'SIGKILL');
  } catch {
    // Already gone.
  }
}

interface HeadlessBackend {
  child: ChildProcess;
  port: number;
  token: string;
  stderr: () => string;
}

// startHeadless boots the backend the Windows launcher starts, with the
// launcher's own flags plus a loopback-only listener.
async function startHeadless(sb: Sandbox): Promise<HeadlessBackend> {
  const child = spawnContained(backend, ['--print-url-fd', '0', '--data-dir', sb.dataDir, '--listen', '127.0.0.1:0',
    '--refuse-pending-migrations'], {
    env: sb.env,
    stdio: ['ignore', 'pipe', 'pipe'],
    memoryLimitBytes: FALLBACK_MEMORY_LIMIT_BYTES,
    detached: true,
  });
  let stderr = '';
  child.stderr!.on('data', (chunk: Buffer) => {
    stderr = (stderr + chunk.toString()).slice(-64 * 1024);
  });
  const bootstrap = await new Promise<{ port: number; token: string }>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`no bootstrap line in 30s\n${stderr}`)), 30_000);
    createInterface({ input: child.stdout! }).on('line', (line) => {
      const at = line.indexOf(BOOTSTRAP_PREFIX);
      if (at < 0) return;
      clearTimeout(timer);
      resolve(JSON.parse(line.slice(at + BOOTSTRAP_PREFIX.length)) as { port: number; token: string });
    });
    child.once('close', (code) => {
      clearTimeout(timer);
      reject(new Error(`the backend exited with ${code} before its bootstrap line\n${stderr}`));
    });
  }).catch((error: Error) => {
    killGroup(child);
    throw error;
  });
  return { child, port: bootstrap.port, token: bootstrap.token, stderr: () => stderr };
}

// settledBootstrap polls /bootstrap.json until the boot stops reporting
// progress, as ProbeBootstrap does, and returns that answer.
async function settledBootstrap(b: HeadlessBackend): Promise<{ status: number; cacheControl: string | null; body: string }> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    const resp = await fetch(`http://127.0.0.1:${b.port}/bootstrap.json`, {
      headers: { authorization: `Bearer ${b.token}` },
    });
    const body = await resp.text();
    if (resp.status !== 503) return { status: resp.status, cacheControl: resp.headers.get('cache-control'), body };
    if (Date.now() > deadline) throw new Error(`the boot was still starting after 60s: ${body}\n${b.stderr()}`);
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

// stopHeadless stops the backend as a signal does and waits for it to exit,
// so the next command finds backend.lock free.
async function stopHeadless(b: HeadlessBackend): Promise<void> {
  if (b.child.exitCode !== null || b.child.signalCode !== null) return;
  b.child.kill('SIGTERM');
  const code = await exited(b.child, 30_000);
  if (code === undefined) throw new Error(`the backend did not stop in 30s after SIGTERM\n${b.stderr()}`);
}

// The calls the fake CLIs recorded, in order.
async function cliCalls(sb: Sandbox): Promise<string[]> {
  return (await readFile(sb.calls, 'utf8').catch(() => '')).split('\n').filter(Boolean);
}

function newUpdateID(): string {
  return randomBytes(8).toString('hex');
}

test('a gated boot refuses a v118 database, a real trial migrates it, and the next boot finds nothing pending', async () => {
  test.setTimeout(240_000);
  const sb = await sandbox();
  const backends: HeadlessBackend[] = [];
  try {
    await writeFixture(sb.dbPath);
    expect(await schemaVersion(sb)).toBe(FIXTURE_VERSION);
    const fixture = await databaseFiles(sb.dbPath);

    // The gate: the boot refuses before anything writes the database.
    const refused = await startHeadless(sb);
    backends.push(refused);
    const refusal = await settledBootstrap(refused);
    expect(refusal.status).toBe(409);
    expect(refusal.cacheControl).toContain('no-store');
    const pending = JSON.parse(refusal.body) as { reason: string; database: number; build: number; pending: number };
    expect(pending).toMatchObject({ reason: 'migrations-pending', database: FIXTURE_VERSION });
    expect(pending.pending).toBe(pending.build - FIXTURE_VERSION);
    expect(pending.pending).toBeGreaterThan(0);
    await stopHeadless(refused);
    expect(await databaseFiles(sb.dbPath)).toEqual(fixture);

    // The migration the launcher runs: snapshot, then the real App trial boot.
    const id = newUpdateID();
    const snapshot = await runUpdateCommand(sb, ['__update-snapshot', '--id', id]);
    expect(result(snapshot.events), snapshot.stderr).toMatchObject({ type: 'result', outcome: 'ok', schema: pending.database });
    const trial = await runUpdateCommand(sb, ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1']);
    expect(result(trial.events), trial.stderr).toMatchObject({ type: 'result', outcome: 'prepared' });
    expect(trial.events.some((event) => event.progress?.phase === 'store.migrate'), trial.stderr).toBe(true);
    expect(await schemaVersion(sb)).toBe(pending.build);
    // Provider probes are unattended work, which a trial parks.
    expect(await cliCalls(sb)).toEqual([]);

    // The live boot after the trial finds nothing pending and starts, and
    // its provider probes resolve to the fakes.
    const live = await startHeadless(sb);
    backends.push(live);
    const ready = await settledBootstrap(live);
    expect(ready.status, ready.body).toBe(200);
    await expect.poll(() => cliCalls(sb), { timeout: 20_000 }).toEqual(
      expect.arrayContaining(['claude --version', 'codex --version']));
    await stopHeadless(live);

    const discard = await runUpdateCommand(sb, ['__update-discard', '--id', id]);
    expect(result(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    await expect(access(path.join(sb.dataDir, 'agent-overflow', 'runtime', 'app-update', 'snapshot'))).rejects.toThrow();
    test.info().annotations.push({ type: 'cli-calls', description: (await cliCalls(sb)).join(' | ') });
  } finally {
    for (const b of backends) {
      await stopHeadless(b).catch(() => killGroup(b.child));
    }
    await rm(sb.root, { recursive: true, force: true });
  }
});

test('a restore after a prepared trial puts back the v118 fixture byte for byte', async () => {
  test.setTimeout(180_000);
  const sb = await sandbox();
  try {
    await writeFixture(sb.dbPath);
    const fixture = await databaseFiles(sb.dbPath);

    const id = newUpdateID();
    const snapshot = await runUpdateCommand(sb, ['__update-snapshot', '--id', id]);
    expect(result(snapshot.events), snapshot.stderr).toMatchObject({ type: 'result', outcome: 'ok', schema: FIXTURE_VERSION });
    const trial = await runUpdateCommand(sb, ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1']);
    expect(result(trial.events), trial.stderr).toMatchObject({ type: 'result', outcome: 'prepared' });
    expect(await schemaVersion(sb)).toBeGreaterThan(FIXTURE_VERSION);
    expect(await databaseFiles(sb.dbPath)).not.toEqual(fixture);

    const restore = await runUpdateCommand(sb, ['__update-restore', '--id', id, '--reason', 'the test rolls back']);
    expect(result(restore.events), restore.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    expect(await databaseFiles(sb.dbPath)).toEqual(fixture);
    expect(await schemaVersion(sb)).toBe(FIXTURE_VERSION);

    const discard = await runUpdateCommand(sb, ['__update-discard', '--id', id]);
    expect(result(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    expect(await cliCalls(sb)).toEqual([]);
  } finally {
    await rm(sb.root, { recursive: true, force: true });
  }
});
