import { spawn } from 'node:child_process';
import { chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { test, expect } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import {
  captureProcessGroupMemberProof,
  captureProcessIdentity,
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
