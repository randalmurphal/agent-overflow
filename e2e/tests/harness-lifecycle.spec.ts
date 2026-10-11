import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { constants } from 'node:fs';
import { access, chmod, mkdir, mkdtemp, open, readdir, readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { pathToFileURL } from 'node:url';
import { test, expect } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessApp, type HarnessLaunchRecord } from '../src/harness.js';
import { BackendConsoleLog } from '../src/harness-console-log.js';
import { launchEnvironment } from '../src/harness-env.js';
import {
  captureProcessGroupMemberProof,
  captureProcessIdentity,
  processTreeRSS,
  terminateChildTreeAndWaitVerified,
  verifyProcessGroupMemberProof,
} from '../src/harness-process.js';

async function fakeBinary(source: string): Promise<{ root: string; binary: string; pidFile: string }> {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-harness-lifecycle-'));
  const binary = path.join(root, 'fake-harness.mjs');
  const pidFile = path.join(root, 'child.pid');
  await writeFile(binary, `#!/usr/bin/env node\n${source}\n`, 'utf8');
  await chmod(binary, 0o700);
  return { root, binary, pidFile };
}

async function processExists(pid: number): Promise<boolean> {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ESRCH') return false;
    throw error;
  }
}

test('refuses a launch when the caller tree is not supervised', async () => {
  const contained = process.env.AO_E2E_CONTAINED;
  const functional = process.env.AO_E2E_FUNCTIONAL_MANAGED;
  delete process.env.AO_E2E_CONTAINED;
  delete process.env.AO_E2E_FUNCTIONAL_MANAGED;
  try {
    await expect(launchHarness()).rejects.toThrow('requires the fixed ao-harness-e2e launcher');
  } finally {
    if (contained === undefined) delete process.env.AO_E2E_CONTAINED;
    else process.env.AO_E2E_CONTAINED = contained;
    if (functional === undefined) delete process.env.AO_E2E_FUNCTIONAL_MANAGED;
    else process.env.AO_E2E_FUNCTIONAL_MANAGED = functional;
  }
});

test('captures the process group a Unix identity is signalled through', async () => {
  test.skip(process.platform === 'win32', 'Windows teardown uses a Job Object, not a process group');
  // Every Unix escalation path authenticates through the group. An identity
  // that omits its group silently declines every member proof, so pin the
  // field itself rather than only the paths that read it.
  const expected = Number(execFileSync('ps', ['-o', 'pgid=', '-p', String(process.pid)], { encoding: 'utf8' }).trim());
  const identity = await captureProcessIdentity(process.pid);
  expect(identity.groupId).toBe(expected);
  expect(identity.executable).not.toBe('');
});

test('sweeps /proc by name so an ordinary exit cannot fail the scan', async () => {
  test.skip(process.platform !== 'linux', 'Linux /proc sweep');
  // A withFileTypes scan lstats every entry procfs leaves untyped, so a
  // process exiting mid-scan raises ENOENT out of the whole readdir. The
  // watchdog reads that as a backend fault and takes the run down with it.
  const source = await readFile(new URL('../src/harness-process.ts', import.meta.url), 'utf8');
  expect(source).toContain("await readdir('/proc')");
  expect(source).not.toMatch(/readdir\('\/proc',/);
});

test('rejects a reused process-group member identity', async () => {
  const identity = await captureProcessIdentity(process.pid);
  await expect(verifyProcessGroupMemberProof({ ...identity, birth: identity.birth + '-reused' })).resolves.toBe(false);
});

test('reaps an owned helper after the process-group leader exits during graceful teardown', async () => {
  test.skip(process.platform === 'win32', 'Unix process-group regression');
  const fake = await fakeBinary(`
    const { spawn } = await import('node:child_process');
    const { writeFileSync } = await import('node:fs');
    const child = spawn(process.execPath, ['-e', 'process.on("SIGTERM", () => {}); setInterval(() => {}, 1000)'], { stdio: 'ignore' });
    writeFileSync(process.env.AO_PID_FILE, String(child.pid));
    process.on('SIGTERM', () => process.exit(0));
    setInterval(() => {}, 1000);
  `);
  const leader = spawn(fake.binary, [], {
    detached: true,
    env: { ...process.env, AO_PID_FILE: fake.pidFile },
    stdio: 'ignore',
  });
  try {
    await expect.poll(async () => {
      try {
        return Number(await readFile(fake.pidFile, 'utf8'));
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 0;
        throw error;
      }
    }).toBeGreaterThan(0);
    const helperPID = Number(await readFile(fake.pidFile, 'utf8'));
    const identity = await captureProcessIdentity(leader.pid);
    const memberProof = await captureProcessGroupMemberProof(identity);
    expect(memberProof?.pid).toBe(helperPID);

    await terminateChildTreeAndWaitVerified(leader, identity, 'SIGTERM', memberProof);
    await expect.poll(() => processExists(helperPID), { timeout: 5_000 }).toBe(false);
  } finally {
    if (leader.pid) {
      try {
        process.kill(-leader.pid, 'SIGKILL');
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
      }
    }
    await rm(fake.root, { recursive: true, force: true });
  }
});

test('reaps a child that prints malformed bootstrap JSON', async () => {
  const fake = await fakeBinary(`
    const { writeFileSync } = await import('node:fs');
    writeFileSync(process.env.AO_PID_FILE, String(process.pid));
    process.stdout.write('__AO_HARNESS__:{not-json}\\n');
    setInterval(() => {}, 1000);
  `);
  try {
    await expect(launchHarness({ binary: fake.binary, dataDir: path.join(fake.root, 'data'), timeoutMs: 1_000, env: { AO_PID_FILE: fake.pidFile } })).rejects.toThrow('unparseable harness bootstrap');
    const pid = Number(await readFile(fake.pidFile, 'utf8'));
    await expect.poll(() => processExists(pid), { timeout: 5_000 }).toBe(false);
  } finally {
    await rm(fake.root, { recursive: true, force: true });
  }
});

test('reaps a child when the bootstrap WebSocket cannot connect', async () => {
  const fake = await fakeBinary(`
    const { writeFileSync } = await import('node:fs');
    const dataDirIndex = process.argv.indexOf('--data-dir');
    const dataRoot = dataDirIndex >= 0 ? process.argv[dataDirIndex + 1] : '';
    writeFileSync(process.env.AO_PID_FILE, String(process.pid));
    process.stdout.write(JSON.stringify({url:'http://127.0.0.1:1/?token=t',port:1,token:'t',dataRoot,dataDir:dataRoot+'/agent-overflow',mockProvider:'mock',pid:process.pid,version:'test'}) + '\\n');
    setInterval(() => {}, 1000);
  `);
  try {
    await expect(launchHarness({ binary: fake.binary, dataDir: path.join(fake.root, 'data'), timeoutMs: 1_000, env: { AO_PID_FILE: fake.pidFile } })).rejects.toThrow();
    const pid = Number(await readFile(fake.pidFile, 'utf8'));
    await expect.poll(() => processExists(pid), { timeout: 5_000 }).toBe(false);
  } finally {
    await rm(fake.root, { recursive: true, force: true });
  }
});

test('reconciles an owned descendant after the bootstrap process exits', async () => {
  const fake = await fakeBinary(`
    const { spawn } = await import('node:child_process');
    const { writeFileSync } = await import('node:fs');
    const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' });
    writeFileSync(process.env.AO_PID_FILE, String(child.pid));
    const dataDirIndex = process.argv.indexOf('--data-dir');
    const dataRoot = dataDirIndex >= 0 ? process.argv[dataDirIndex + 1] : '';
    process.stdout.write(JSON.stringify({url:'http://127.0.0.1:1/?token=t',port:1,token:'t',dataRoot,dataDir:dataRoot+'/agent-overflow',mockProvider:'mock',pid:process.pid,version:'test'}) + '\\n');
  `);
  try {
    await expect(launchHarness({ binary: fake.binary, dataDir: path.join(fake.root, 'data'), timeoutMs: 1_000, env: { AO_PID_FILE: fake.pidFile } })).rejects.toThrow();
    const pid = Number(await readFile(fake.pidFile, 'utf8'));
    await expect.poll(() => processExists(pid), { timeout: 5_000 }).toBe(false);
  } finally {
    await rm(fake.root, { recursive: true, force: true });
  }
});

// The per-pid identity and the process-tree snapshot come from two
// different `ps` invocations on darwin, and the watchdog kills a harness
// whose birth marker disagrees between them. This runs both against the
// test's own process, so a parser that spells the marker differently
// (2026-09-03: `lstart` pads a single-digit day with two spaces, and the
// tree parser rejoined the split fields with one) fails here instead of
// killing every backend the suite launches.
test('the tree snapshot recognises the identity the per-pid capture took', async () => {
  const identity = await captureProcessIdentity(process.pid);
  await expect(processTreeRSS(identity)).resolves.toBeGreaterThan(0);
});

function consoleLogPath(dataRoot: string): string {
  return path.join(dataRoot, 'agent-overflow', 'logs', BackendConsoleLog.fileName);
}

// Descriptors this worker holds on `file`, removed or not. Windows cannot
// remove a root holding one, so every removal path must close it first.
async function descriptorsOn(file: string): Promise<number> {
  let count = 0;
  for (const fd of await readdir('/proc/self/fd')) {
    const target = await readlink(`/proc/self/fd/${fd}`).catch(() => '');
    if (target === file || target === `${file} (deleted)`) count += 1;
  }
  return count;
}

// launchHarness registers the console log's readline on the backend's
// stderr first. A listener added later sees each chunk after it, so once
// this one has seen `text` and the newline ending its line, the console log
// has been handed that line, which need not be in the file yet. `child` is
// private to HarnessApp; only this spec reaches it.
function stderrLine(harness: HarnessApp, text: string): Promise<void> {
  const stderr = (harness as unknown as { child: ChildProcess }).child.stderr!;
  return new Promise((resolve, reject) => {
    let pending = '';
    let seen = false;
    const finish = (error?: Error) => {
      stderr.off('data', onData);
      stderr.off('end', onEnd);
      if (error) reject(error);
      else resolve();
    };
    const onData = (chunk: Buffer) => {
      pending += chunk.toString('latin1');
      if (!seen) {
        const at = pending.indexOf(text);
        if (at === -1) {
          pending = pending.slice(-text.length);
          return;
        }
        seen = true;
        pending = pending.slice(at + text.length);
      }
      if (pending.includes('\n')) finish();
      else pending = '';
    };
    const onEnd = () => finish(new Error(`backend stderr ended before a line holding ${text}`));
    stderr.on('data', onData);
    stderr.once('end', onEnd);
  });
}

// tests/fixtures.ts calls writeDiagnostics when a test fails while holding
// the worker backend. This pins what lands beside the trace.
test('writes a backend console, launch record and own logs as diagnostics', async ({}, testInfo) => {
  const marker = `diagnostics-probe-${process.pid}-${Date.now()}`;
  const harness = await launchHarness({ env: { DIAGNOSTICS_PROBE: marker } });
  try {
    // An RPC error is logged with its text. Diagnostics start the moment
    // that line is handed to the console log, before the call's reply, and
    // a 32 MiB name keeps its write to the file in flight then, so
    // backend.log holds it only because diagnostics flush first.
    const dir = testInfo.outputPath('diagnostics');
    const logged = stderrLine(harness, `/${marker}`);
    const rejected = expect(harness.rpc('HarnessReplayBundle', `${'x'.repeat(32 << 20)}/${marker}`, {})).rejects.toThrow(marker);
    await logged;
    await harness.writeDiagnostics(dir);
    await rejected;

    const backendLog = await readFile(path.join(dir, 'backend.log'), 'utf8');
    expect(backendLog).toContain(`/${marker}" must be a plain directory name`);
    expect(backendLog).toContain(`"pid":${harness.bootstrap.pid}`);
    expect(backendLog).toMatch(/^\[stdout\] __AO_HARNESS__:/m);
    expect(backendLog).toContain(`transport: harness mode (data dir ${harness.bootstrap.dataDir})`);

    const record = JSON.parse(await readFile(path.join(dir, 'backend-launch.json'), 'utf8')) as HarnessLaunchRecord & {
      pid: number;
      dataRoot: string;
      dataDir: string;
    };
    expect(record.pid).toBe(harness.bootstrap.pid);
    expect(record.dataRoot).toBe(harness.bootstrap.dataRoot);
    expect(record.dataDir).toBe(harness.bootstrap.dataDir);
    expect(record.args).toEqual(expect.arrayContaining(['--harness', '--data-dir', harness.bootstrap.dataRoot]));
    expect(record.harnessTiming).toBe(HARNESS_TIMING);
    // The record is the environment the backend was given, not a rebuild.
    expect(record.env).toEqual(launchEnvironment({ AO_HARNESS_TIMING: HARNESS_TIMING, DIAGNOSTICS_PROBE: marker }));
    expect(record.env.AO_HARNESS_TIMING).toBe(HARNESS_TIMING);
    expect(record.env.DIAGNOSTICS_PROBE).toBe(marker);
    expect(record.env.AO_E2E_CONTAINED).toBe('1');
    expect(record.env.PATH).toBe(process.env.PATH);

    const ownLogs = await readdir(path.join(dir, 'backend-logs'));
    expect(ownLogs).toContainEqual(expect.stringMatching(/^engine-\d{4}-\d{2}-\d{2}\.ndjson$/));
    expect(ownLogs).not.toContain(BackendConsoleLog.fileName);
    if (process.platform === 'linux') expect(await descriptorsOn(consoleLogPath(harness.bootstrap.dataRoot))).toBe(1);
  } finally {
    await harness.close();
  }
  await expect(access(harness.bootstrap.dataRoot)).rejects.toMatchObject({ code: 'ENOENT' });
  if (process.platform === 'linux') expect(await descriptorsOn(consoleLogPath(harness.bootstrap.dataRoot))).toBe(0);
});

// An agent session running the suite exports its live AO_TOKEN, and a
// developer's shell holds provider and service keys. None may reach a
// backend the suite starts.
test('a launched backend inherits only the allowlisted environment', async ({}, testInfo) => {
  const canaries = { AO_TOKEN: 'canary-token', FAKE_SECRET: 'canary-secret' };
  const previous = Object.fromEntries(Object.keys(canaries).map((name) => [name, process.env[name]]));
  Object.assign(process.env, canaries);
  let harness: Awaited<ReturnType<typeof launchHarness>>;
  try {
    harness = await launchHarness();
  } finally {
    for (const [name, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
  try {
    if (process.platform === 'linux') {
      // The backend's own exec-time environment, not the launcher's intent.
      const environ = (await readFile(`/proc/${harness.bootstrap.pid}/environ`, 'utf8')).split('\0');
      expect(environ.filter((entry) => entry.includes('canary'))).toEqual([]);
      expect(environ).toContain(`AO_HARNESS_TIMING=${HARNESS_TIMING}`);
      expect(environ).toContain(`PATH=${process.env.PATH}`);
    }
    const dir = testInfo.outputPath('diagnostics');
    await harness.writeDiagnostics(dir);
    const record = await readFile(path.join(dir, 'backend-launch.json'), 'utf8');
    expect(record).not.toContain('canary');
  } finally {
    await harness.close();
  }
});

test('the launch environment keeps the allowlist and every override', async () => {
  const source = {
    PATH: '/bin',
    HOME: '/home/dev',
    LC_ALL: 'C.UTF-8',
    AO_E2E_WORKERS: '2',
    AO_HARNESS_DEBUG: '1',
    AO_HARNESS_KEEP_HOME: '1',
    AO_MOCKPROVIDER: '/mock',
    AO_TOKEN: 'canary',
    AO_ENDPOINT: 'canary',
    AO_THREAD_ID: 'canary',
    AO_PROJECT: 'canary',
    ATLASSIAN_API_TOKEN: 'canary',
    ANTHROPIC_API_KEY: 'canary',
    SHELL: '/bin/zsh',
    WSL_DISTRO_NAME: 'canary',
    XDG_CONFIG_HOME: '/home/dev/.config',
    AGENT_OVERFLOW_WIN_APPDATA: 'canary',
    SystemRoot: 'C:\\Windows',
  };
  expect(launchEnvironment({ AO_PID_FILE: '/pid', HOME: '/harness-home' }, source, 'linux')).toEqual({
    PATH: '/bin',
    HOME: '/harness-home',
    LC_ALL: 'C.UTF-8',
    AO_E2E_WORKERS: '2',
    AO_HARNESS_DEBUG: '1',
    AO_MOCKPROVIDER: '/mock',
    AO_PID_FILE: '/pid',
  });
  // AO_HARNESS_KEEP_HOME would hand the backend the real HOME, so only a
  // launcher that names it passes it.
  expect(launchEnvironment({ AO_HARNESS_KEEP_HOME: '1' }, source, 'linux').AO_HARNESS_KEEP_HOME).toBe('1');
  // Windows names fold case and keep the spelling Node reports.
  const windows = launchEnvironment(
    { PATH: 'C:\\fixture;C:\\Windows' },
    { Path: 'C:\\Windows', SystemRoot: 'C:\\Windows', Temp: 'C:\\Temp', AO_TOKEN: 'canary', Ao_Harness_Keep_Home: '1' },
    'win32',
  );
  expect(windows).toEqual({ SystemRoot: 'C:\\Windows', Temp: 'C:\\Temp', PATH: 'C:\\fixture;C:\\Windows' });
});

test('a launch that cannot spawn closes its console log', async () => {
  test.skip(process.platform !== 'linux', 'the 1 KiB floor is checked before spawn only on the Linux rlimit path');
  const root = await mkdtemp(path.join(tmpdir(), 'ao-harness-console-'));
  try {
    await expect(launchHarness({ dataDir: root, memoryLimitBytes: 1 })).rejects.toThrow('at least 1 KiB');
    expect(await descriptorsOn(consoleLogPath(root))).toBe(0);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test('flush puts every written console line in the file', async () => {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-harness-console-'));
  try {
    const log = await BackendConsoleLog.open(root);
    const lines = Array.from({ length: 5_000 }, (_, index) => `line ${index}`);
    for (const line of lines) log.write(line);
    await log.flush();
    expect(await readFile(log.path, 'utf8')).toBe(`${lines.join('\n')}\n`);
    await log.close();
    log.write('after close');
    await log.flush();
    expect(await readFile(log.path, 'utf8')).toBe(`${lines.join('\n')}\n`);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test('close leaves a kept data root and its console in place', async () => {
  const kept = await launchHarness();
  try {
    kept.keepDataRoot();
    await kept.close();
    const consoleText = await readFile(consoleLogPath(kept.bootstrap.dataRoot), 'utf8');
    expect(consoleText).toContain(`"pid":${kept.bootstrap.pid}`);
    expect(consoleText).toContain('received terminated, shutting down');
  } finally {
    await rm(kept.bootstrap.dataRoot, { recursive: true, force: true });
  }
});

// close() returns only once the console log has written every line the
// backend printed and closed its file. Teardown's wait proves only that the
// process exited; the log's writes can still be queued. A FIFO at the
// console path holds those writes until the test reads it.
test('close waits for the console log to write every line and close', async () => {
  test.skip(process.platform === 'win32', 'holds the console log on a FIFO');
  const root = await mkdtemp(path.join(tmpdir(), 'ao-harness-drain-'));
  const fifo = consoleLogPath(root);
  await mkdir(path.dirname(fifo), { recursive: true });
  execFileSync('mkfifo', [fifo]);
  // An open reader lets the console log open the FIFO; nothing reads it yet.
  const reader = await open(fifo, constants.O_RDONLY | constants.O_NONBLOCK);
  const reported: string[] = [];
  const consoleError = console.error;
  console.error = (...args: unknown[]) => {
    reported.push(args.map(String).join(' '));
    consoleError(...args);
  };
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir: root });
    const marker = `drain-probe-${process.pid}-${Date.now()}`;
    // A line longer than the FIFO's buffer leaves the console's writes waiting.
    await expect(harness.rpc('HarnessReplayBundle', `${'x'.repeat(1 << 20)}/${marker}`, {})).rejects.toThrow(marker);
    expect(await harness.stop()).toBe(true);
    let returned = false;
    const closing = harness.close().then(() => {
      returned = true;
    });
    // After a completed stop(), everything close() does on a root it does
    // not own, other than waiting for the console, settles in microtasks.
    await new Promise((resolve) => setImmediate(resolve));
    expect(returned).toBe(false);
    const drained = readFile(fifo, 'latin1');
    await closing;
    const text = await drained;
    expect(text).toContain(`/${marker}" must be a plain directory name`);
    expect(text).toContain('received terminated, shutting down');
    // launchHarness ends the log when the child's pipes close; without that,
    // close() waits out its bound and reports the drain as timed out.
    expect(reported.filter((line) => line.includes('timed out'))).toEqual([]);
  } finally {
    console.error = consoleError;
    await reader.close();
    await harness?.close();
    await rm(root, { recursive: true, force: true });
  }
});

// Runs `body` in a separate Node process that imports the harness, so what
// happens at that process's exit can be observed. `body` ends by printing
// one JSON line and calling process.exit, as a Playwright worker exits.
async function harnessScript(body: string): Promise<{ result: { root: string; pid: number }; stderr: string }> {
  const dir = await mkdtemp(path.join(tmpdir(), 'ao-harness-exit-'));
  try {
    const script = path.join(dir, 'script.mts');
    const harness = pathToFileURL(path.resolve(import.meta.dirname, '..', 'src', 'harness.ts')).href;
    await writeFile(script, `import { launchHarness } from ${JSON.stringify(harness)};\n${body}\n`);
    const child = spawn(process.execPath, ['--experimental-transform-types', '--no-warnings', script], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    child.stdout!.on('data', (chunk: Buffer) => (stdout += chunk.toString()));
    child.stderr!.on('data', (chunk: Buffer) => (stderr += chunk.toString()));
    const code = await new Promise<number | null>((resolve, reject) => {
      child.once('error', reject);
      child.once('close', resolve);
    });
    if (code !== 0) throw new Error(`harness script exited ${code}: ${stderr}`);
    return { result: JSON.parse(stdout.trim().split('\n').at(-1)!), stderr };
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

test('an owned root a spec only stopped is removed when the process exits', async () => {
  const { result } = await harnessScript(`
    const harness = await launchHarness();
    if (!(await harness.stop())) throw new Error('stop failed');
    console.log(JSON.stringify({ root: harness.bootstrap.dataRoot, pid: harness.bootstrap.pid }));
    process.exit(0);
  `);
  try {
    await expect(access(result.root)).rejects.toMatchObject({ code: 'ENOENT' });
  } finally {
    await rm(result.root, { recursive: true, force: true });
  }
});

test('an owned root whose backend was never stopped is left and reported at exit', async () => {
  test.skip(process.platform === 'win32', 'stops the orphaned backend through its Unix process group');
  const { result, stderr } = await harnessScript(`
    const harness = await launchHarness();
    console.log(JSON.stringify({ root: harness.bootstrap.dataRoot, pid: harness.bootstrap.pid }));
    process.exit(0);
  `);
  try {
    await access(result.root);
    expect(stderr).toContain(`harness data root ${result.root} left at exit: backend`);
  } finally {
    // The backend outlived the script in its own process group.
    try {
      process.kill(-result.pid, 'SIGKILL');
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
    }
    await rm(result.root, { recursive: true, force: true });
  }
});
