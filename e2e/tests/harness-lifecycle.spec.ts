import { execFileSync, spawn } from 'node:child_process';
import { access, chmod, mkdtemp, readdir, readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { test, expect } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessLaunchRecord } from '../src/harness.js';
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

// tests/fixtures.ts calls writeDiagnostics when a test fails while holding
// the worker backend. This pins what lands beside the trace.
test('writes a backend console, launch record and own logs as diagnostics', async ({}, testInfo) => {
  const marker = `diagnostics-probe-${process.pid}-${Date.now()}`;
  const harness = await launchHarness({ env: { DIAGNOSTICS_PROBE: marker } });
  try {
    // An RPC error is logged with its text, so the marker reaches stderr
    // only after a backend call.
    await expect(harness.rpc('HarnessReplayBundle', marker, {})).rejects.toThrow(marker);
    await expect.poll(async () => readFile(consoleLogPath(harness.bootstrap.dataRoot), 'utf8')).toContain(marker);

    const dir = testInfo.outputPath('diagnostics');
    await harness.writeDiagnostics(dir);

    const backendLog = await readFile(path.join(dir, 'backend.log'), 'utf8');
    expect(backendLog).toContain(`"pid":${harness.bootstrap.pid}`);
    expect(backendLog).toMatch(/^\[stdout\] __AO_HARNESS__:/m);
    expect(backendLog).toContain(`transport: harness mode (data dir ${harness.bootstrap.dataDir})`);
    expect(backendLog).toContain(`bundle "${marker}" has no meta.json`);

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
    AO_HARNESS_KEEP_HOME: '1',
    AO_MOCKPROVIDER: '/mock',
    AO_PID_FILE: '/pid',
  });
  // Windows names fold case and keep the spelling Node reports.
  const windows = launchEnvironment(
    { PATH: 'C:\\fixture;C:\\Windows' },
    { Path: 'C:\\Windows', SystemRoot: 'C:\\Windows', Temp: 'C:\\Temp', AO_TOKEN: 'canary' },
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
    // close() waits for the pipes to drain, so the shutdown is recorded.
    expect(consoleText).toContain('received terminated, shutting down');
  } finally {
    await rm(kept.bootstrap.dataRoot, { recursive: true, force: true });
  }
});
