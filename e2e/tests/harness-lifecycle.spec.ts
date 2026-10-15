import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { chmod, mkdtemp, readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import type { Duplex } from 'node:stream';
import { test, expect } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { HarnessApp } from '../src/harness-app.js';
import {
  captureProcessGroupMemberProof,
  captureProcessIdentity,
  captureProcessTreeProof,
  ownedProcessTreeAlive,
  processHoldState,
  processTreeRSS,
  terminateChildTreeAndWaitVerified,
} from '../src/harness-process.js';
import { FALLBACK_MEMORY_LIMIT_BYTES, HarnessWatchdog } from '../src/harness-watchdog.js';

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
  await expect(processHoldState({ ...identity, birth: identity.birth + '-reused' })).resolves.toBe('changed');
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

test('escalates through a tree-proof member after the leader exits during the grace window', async () => {
  test.skip(process.platform === 'win32', 'Unix process-group regression');
  // The lifecycle browser passes only a tree proof. The leader exits on
  // SIGTERM while a helper ignores it, so escalation must authenticate the
  // group through the helper's tree-proof identity.
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
    const treeProof = await captureProcessTreeProof(identity);
    expect(treeProof.descendants.map((proof) => proof.pid)).toContain(helperPID);

    await terminateChildTreeAndWaitVerified(leader, identity, 'SIGTERM', undefined, treeProof);
    expect(await processExists(helperPID)).toBe(false);
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

test('an exiting process still holds its pid while its exe link is gone', async () => {
  test.skip(process.platform !== 'linux', 'Linux exe link');
  // A process mid-exit or a zombie has no readable exe link, but its stat
  // still carries the captured birth and group. Treating that as a changed
  // identity refused escalation for a Chromium leader that was only exiting.
  // The exec'd parent never reaps, so the child stays a zombie.
  const parent = spawn('/bin/sh', ['-c', 'setsid sleep 1 & echo $!; exec sleep 30'], {
    stdio: ['ignore', 'pipe', 'ignore'],
  });
  try {
    const pid = Number((await once(parent.stdout!, 'data'))[0].toString().trim());
    const identity = await captureProcessIdentity(pid);
    expect(identity.groupId).toBe(pid);
    await expect.poll(async () => (await readFile(`/proc/${pid}/stat`, 'utf8')).split(') ')[1]?.[0]).toBe('Z');
    await expect(readlink(`/proc/${pid}/exe`)).rejects.toThrow();

    await expect(processHoldState(identity)).resolves.toBe('held');
    await expect(processHoldState({ ...identity, birth: `${identity.birth}1` })).resolves.toBe('changed');
    // Teardown liveness applies the same rule: an exiting process can still
    // hold files, so its data root is not reclaimed yet.
    const child = { pid, exitCode: 0, signalCode: null } as unknown as ChildProcess;
    await expect(ownedProcessTreeAlive(child, identity)).resolves.toBe(true);
    parent.kill('SIGKILL');
    await expect.poll(() => processHoldState(identity), { timeout: 5_000 }).toBe('released');
    await expect(ownedProcessTreeAlive(child, identity)).resolves.toBe(false);
  } finally {
    parent.kill('SIGKILL');
  }
});

test('the watchdog samples a backend that is mid-exit and stops once it is reaped', async () => {
  test.skip(process.platform !== 'linux', 'Linux exe link and VmRSS');
  // An exiting backend has no exe link and no VmRSS but still holds its pid.
  // Reading that as a changed identity tripped the watchdog with a false
  // reason and tore the backend down. The exec'd parent never reaps, so the
  // sampled process stays a zombie until the parent is killed.
  const root = await mkdtemp(path.join(tmpdir(), 'ao-watchdog-exit-'));
  const parent = spawn('/bin/sh', ['-c', 'setsid sleep 2 & echo $!; exec sleep 30'], {
    stdio: ['ignore', 'pipe', 'ignore'],
  });
  let closed = false;
  let watchdog: HarnessWatchdog | undefined;
  try {
    const pid = Number((await once(parent.stdout!, 'data'))[0].toString().trim());
    const child = { pid, exitCode: null, signalCode: null } as unknown as ChildProcess;
    watchdog = new HarnessWatchdog({
      child,
      dataRoot: root,
      dataDir: root,
      isClosed: () => closed,
      markClosed: () => { closed = true; },
      shutdown: async () => undefined,
      closeSocket: () => undefined,
      socketOpen: () => false,
    });
    await watchdog.start(FALLBACK_MEMORY_LIMIT_BYTES);
    // Drive samples directly so each assertion covers one known state; an
    // interval sample in flight would make a direct one return early.
    watchdog.stop();
    const sampled = watchdog as unknown as {
      sample(limit: number): Promise<void>;
      timer?: ReturnType<typeof setInterval>;
    };
    await expect.poll(async () => (await readFile(`/proc/${pid}/stat`, 'utf8')).split(') ')[1]?.[0], { timeout: 5_000 }).toBe('Z');
    await sampled.sample(FALLBACK_MEMORY_LIMIT_BYTES);
    expect(closed).toBe(false);

    parent.kill('SIGKILL');
    await expect.poll(() => processHoldState(watchdog!.processIdentity!), { timeout: 5_000 }).toBe('released');
    sampled.timer = setInterval(() => undefined, 60_000);
    await sampled.sample(FALLBACK_MEMORY_LIMIT_BYTES);
    expect(closed).toBe(false);
    expect(sampled.timer).toBeUndefined();
  } finally {
    watchdog?.stop();
    parent.kill('SIGKILL');
    await rm(root, { recursive: true, force: true });
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

type ShutdownAnswer = 'close' | 'error-reply';

/**
 * A backend stand-in on a real WebSocket. On HarnessShutdown it either closes
 * the socket without a reply, as the real backend does when its shutdown wins
 * the race against the reply write, or replies with an RPC error. After a
 * close its owned process exits on its own one second later: longer than the
 * half second an unacknowledged shutdown waits before signalling, shorter than
 * the wait an acknowledged one gets.
 */
async function fakeShutdownBackend(answer: ShutdownAnswer): Promise<{
  app: HarnessApp;
  child: ChildProcess;
  close: () => Promise<void>;
}> {
  const child = spawn(
    process.execPath,
    ['-e', 'process.on("message", () => process.exit(0)); setInterval(() => {}, 1000)'],
    { detached: process.platform !== 'win32', stdio: ['ignore', 'ignore', 'ignore', 'ipc'] },
  );
  const server = createServer();
  let exitTimer: ReturnType<typeof setTimeout> | undefined;
  const sockets = new Set<Duplex>();
  server.on('upgrade', (request, socket) => {
    sockets.add(socket);
    const key = String(request.headers['sec-websocket-key']);
    const accept = createHash('sha1').update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(
      'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n' +
        `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
    );
    let buffered = Buffer.alloc(0);
    socket.on('data', (chunk: Buffer) => {
      buffered = Buffer.concat([buffered, chunk]);
      for (;;) {
        if (buffered.length < 2) return;
        let length = buffered[1] & 0x7f;
        let offset = 2;
        if (length === 126) {
          if (buffered.length < 4) return;
          length = buffered.readUInt16BE(2);
          offset = 4;
        } else if (length === 127) {
          if (buffered.length < 10) return;
          length = Number(buffered.readBigUInt64BE(2));
          offset = 10;
        }
        if (buffered.length < offset + 4 + length) return;
        const mask = buffered.subarray(offset, offset + 4);
        const payload = Buffer.from(buffered.subarray(offset + 4, offset + 4 + length));
        for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
        const opcode = buffered[0] & 0x0f;
        buffered = buffered.subarray(offset + 4 + length);
        if (opcode !== 0x1) continue;
        const frame = JSON.parse(payload.toString('utf8')) as { id: string; method: string };
        if (frame.method !== 'HarnessShutdown') continue;
        if (answer === 'error-reply') {
          const reply = Buffer.from(
            JSON.stringify({ type: 'rpc', id: frame.id, error: { code: 'internal', message: 'shutdown refused' } }),
          );
          socket.write(Buffer.concat([Buffer.from([0x81, reply.length]), reply]));
          continue;
        }
        socket.destroy();
        exitTimer = setTimeout(() => child.send('exit'), 1_000);
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  const identity = await captureProcessIdentity(child.pid);
  const app = new HarnessApp(
    child,
    {
      url: `http://127.0.0.1:${port}/`,
      port,
      token: 't',
      dataRoot: '',
      dataDir: '',
      mockProvider: '',
      mockForge: '',
      pid: child.pid ?? 0,
      version: 'test',
    },
    undefined,
    undefined,
    undefined,
    identity,
  );
  await app.connect();
  return {
    app,
    child,
    close: async () => {
      clearTimeout(exitTimer);
      // Upgraded sockets are no longer the server's; close would wait on them.
      for (const socket of sockets) socket.destroy();
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

test('stopping a backend inside a test attaches its stderr tail to the test', async () => {
  const app = await launchHarness();
  await app.close();
  const attachment = test.info().attachments.find((entry) => entry.name === `backend ${app.bootstrap.pid} stderr`);
  expect(attachment?.contentType).toBe('text/plain');
  expect(attachment?.body?.toString()).toMatch(/store: migration v\d+ applied/);
});

test('a backend closing the socket on HarnessShutdown is a graceful stop, not a failure', async () => {
  test.skip(process.platform === 'win32', 'Windows teardown reconciles the Job Object tree after any shutdown');
  const backend = await fakeShutdownBackend('close');
  const errors: string[] = [];
  const consoleError = console.error;
  console.error = (...args: unknown[]) => {
    errors.push(args.map(String).join(' '));
  };
  try {
    await expect(backend.app.stop()).resolves.toBe(true);
  } finally {
    console.error = consoleError;
    await backend.close();
  }
  expect(errors).toEqual([]);
  // The backend exited on its own; teardown never signalled it.
  expect(backend.child.signalCode).toBeNull();
  expect(backend.child.exitCode).toBe(0);
});

test('a HarnessShutdown error reply or a socket closed before the request stays a failure', async () => {
  const refusing = await fakeShutdownBackend('error-reply');
  try {
    await expect(refusing.app.requestShutdown()).rejects.toThrow('internal: shutdown refused');
  } finally {
    await refusing.close();
  }

  const closing = await fakeShutdownBackend('close');
  try {
    // The close answers only the shutdown; another call it cuts off fails.
    const unanswered = closing.app.rpc('HarnessInfo');
    await expect(closing.app.requestShutdown()).resolves.toBeUndefined();
    await expect(unanswered).rejects.toThrow('harness WS closed unexpectedly');
    // The socket is gone, so a second request is never written.
    await expect(closing.app.requestShutdown()).rejects.toThrow('rpc HarnessShutdown: WS is not connected');
  } finally {
    await closing.close();
  }
});
