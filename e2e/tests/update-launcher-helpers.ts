// The Windows launcher's side of an update, driven the way the launcher
// drives it: a from-nothing sandbox every backend process runs in, the
// update commands (__update-snapshot, __update-trial-run, __update-restore,
// __update-discard) run as wsllauncher.UpdateCommandRunner runs them, and
// the gated headless boot (--refuse-pending-migrations) probed as the
// launcher probes it. update-trial.spec.ts drives the v118 fixture through
// it; the release upgrade check drives the corpus through it and holds
// every run to the launcher's stall window, ceiling and probe deadline.
import { type ChildProcess } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { createInterface } from 'node:readline';
import { spawnContained } from '../src/harness-process.js';
import { FALLBACK_MEMORY_LIMIT_BYTES } from '../src/harness-watchdog.js';

/** The backend under test: bin/agent-overflow from `make harness-build`, or AO_HARNESS_BIN. */
export const backendBinary = process.env.AO_HARNESS_BIN
  ?? path.join(path.resolve(import.meta.dirname, '..', '..'), 'bin', 'agent-overflow');

const UPDATE_PREFIX = '__AO_UPDATE__:';
const BOOTSTRAP_PREFIX = '__AO_BOOTSTRAP__:';

export interface Sandbox {
  /** Everything a run writes lives under here. */
  root: string;
  /** The data root the backends boot on (`--data-dir`). */
  dataRoot: string;
  /** `<dataRoot>/agent-overflow/agent-overflow.db`. */
  dbPath: string;
  /** The from-nothing environment every backend process runs with. */
  env: NodeJS.ProcessEnv;
  /** Where the fake provider and forge CLIs record their calls. */
  calls: string;
}

/**
 * buildSandbox fills an existing, empty `root` with a data root, a home
 * and fake claude, codex, gh and glab CLIs that record their calls. The
 * environment is built from nothing, so no provider home override, WSL
 * interop variable or user PATH entry reaches a backend. The login-shell
 * PATH probe runs a stand-in shell that skips login files, where a system
 * profile could put a real provider CLI ahead of the fakes.
 */
export async function buildSandbox(root: string): Promise<Sandbox> {
  const home = path.join(root, 'home');
  const bin = path.join(root, 'bin');
  const dataRoot = path.join(root, 'data');
  const calls = path.join(root, 'cli-calls.log');
  const shell = path.join(root, 'login-shell');
  await mkdir(home, { recursive: true });
  await mkdir(bin, { recursive: true });
  await writeFile(shell, ['#!/bin/sh', '[ "$1" = -ilc ] && shift', 'exec /bin/sh -c "$1"', ''].join('\n'));
  await chmod(shell, 0o755);
  await mkdir(path.join(dataRoot, 'agent-overflow'), { recursive: true });
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
    SHELL: shell,
    PATH: `${bin}:/usr/bin:/bin`,
    LANG: 'C.UTF-8',
    TMPDIR: path.join(root, 'tmp'),
  };
  await mkdir(env.TMPDIR!, { recursive: true });
  return { root, dataRoot, dbPath: path.join(dataRoot, 'agent-overflow', 'agent-overflow.db'), env, calls };
}

/** The calls the fake CLIs recorded, in order. */
export async function cliCalls(sb: Sandbox): Promise<string[]> {
  return (await readFile(sb.calls, 'utf8').catch(() => '')).split('\n').filter(Boolean);
}

export function newUpdateID(): string {
  return randomBytes(8).toString('hex');
}

export interface UpdateEvent {
  type: 'started' | 'progress' | 'result';
  outcome?: string;
  reason?: string;
  schema?: number;
  progress?: { phase: string; detail: string };
  /** When the check read the report, for the stall-rule mirror. */
  at: number;
}

export interface Cadence {
  /** The longest gap between two consecutive reports. */
  maxGapMs: number;
  totalMs: number;
}

/**
 * assertCadence holds a run to the stall rule its production judge
 * applies: no gap between reports longer than the window, and the whole
 * run under the ceiling. A migration that passes here would also pass the
 * launcher; one that fails here would have been killed as stalled on a
 * user's machine.
 */
export function assertCadence(label: string, events: UpdateEvent[], startedAt: number, endedAt: number, windowMs: number, ceilingMs: number): Cadence {
  let last = startedAt;
  let maxGapMs = 0;
  for (const event of [...events, { at: endedAt } as UpdateEvent]) {
    maxGapMs = Math.max(maxGapMs, event.at - last);
    last = event.at;
  }
  const totalMs = endedAt - startedAt;
  if (maxGapMs > windowMs) {
    throw new Error(`${label} went ${maxGapMs}ms without a progress report; the launcher judges a run stalled after ${windowMs}ms`);
  }
  if (totalMs > ceilingMs) {
    throw new Error(`${label} took ${totalMs}ms; the launcher's ceiling is ${ceilingMs}ms`);
  }
  return { maxGapMs, totalMs };
}

/**
 * runUpdateCommand runs one update command of the candidate to its end,
 * as wsllauncher.UpdateCommandRunner does, and returns its reports.
 */
export async function runUpdateCommand(
  sb: Sandbox,
  args: string[],
  timeoutMs: number,
): Promise<{ events: UpdateEvent[]; stderr: string }> {
  const child = spawnContained(backendBinary, [...args, '--data-dir', sb.dataRoot], {
    env: sb.env,
    stdio: ['ignore', 'pipe', 'pipe'],
    memoryLimitBytes: FALLBACK_MEMORY_LIMIT_BYTES,
    detached: true,
  });
  const events: UpdateEvent[] = [];
  let stderr = '';
  createInterface({ input: child.stdout! }).on('line', (line) => {
    const at = line.indexOf(UPDATE_PREFIX);
    if (at >= 0) events.push({ ...(JSON.parse(line.slice(at + UPDATE_PREFIX.length)) as Omit<UpdateEvent, 'at'>), at: Date.now() });
  });
  child.stderr!.on('data', (chunk: Buffer) => {
    stderr = (stderr + chunk.toString()).slice(-64 * 1024);
  });
  const code = await exited(child, timeoutMs);
  if (code === undefined) throw new Error(`${args[0]} did not finish in ${timeoutMs}ms\n${stderr}`);
  return { events, stderr };
}

export function updateResult(events: UpdateEvent[]): UpdateEvent | undefined {
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

export function killGroup(child: ChildProcess): void {
  if (child.pid === undefined) return;
  try {
    process.kill(-child.pid, 'SIGKILL');
  } catch {
    // Already gone.
  }
}

export interface HeadlessBackend {
  child: ChildProcess;
  port: number;
  token: string;
  stderr: () => string;
}

/**
 * startHeadless boots the backend the way the Windows launcher starts it
 * after an update: the launcher's own flags, the migration gate, a
 * loopback-only listener. Fails when no bootstrap line arrives within
 * `bootstrapTimeoutMs`.
 */
export async function startHeadless(sb: Sandbox, bootstrapTimeoutMs: number): Promise<HeadlessBackend> {
  const child = spawnContained(backendBinary, ['--print-url-fd', '0', '--data-dir', sb.dataRoot, '--listen', '127.0.0.1:0',
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
    const timer = setTimeout(() => reject(new Error(`no bootstrap line in ${bootstrapTimeoutMs}ms\n${stderr}`)), bootstrapTimeoutMs);
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

export interface SettledBootstrap {
  status: number;
  cacheControl: string | null;
  body: string;
  /** The longest the starting report went unchanged, what the launcher's probe judges. */
  maxProgressGapMs: number;
}

/**
 * settledBootstrap polls /bootstrap.json until the boot stops reporting
 * progress, as ProbeBootstrap does, and returns that answer. The launcher
 * gives up when the starting report stops changing for its deadline, so
 * a gap longer than `progressDeadlineMs` fails here as it would there. A
 * boot on a large database spends its starting phase in the boot-time
 * checkpoint and sweeps, so the overall deadline is the caller's.
 */
export async function settledBootstrap(b: HeadlessBackend, timeoutMs: number, progressDeadlineMs: number): Promise<SettledBootstrap> {
  const deadline = Date.now() + timeoutMs;
  let lastBody = '';
  let lastChange = Date.now();
  let maxProgressGapMs = 0;
  for (;;) {
    const resp = await fetch(`http://127.0.0.1:${b.port}/bootstrap.json`, {
      headers: { authorization: `Bearer ${b.token}` },
    });
    const body = await resp.text();
    const now = Date.now();
    if (body !== lastBody) {
      maxProgressGapMs = Math.max(maxProgressGapMs, now - lastChange);
      lastBody = body;
      lastChange = now;
    }
    if (resp.status !== 503) return { status: resp.status, cacheControl: resp.headers.get('cache-control'), body, maxProgressGapMs };
    if (now - lastChange > progressDeadlineMs) {
      throw new Error(`the boot reported no progress for ${now - lastChange}ms; the launcher gives up after ${progressDeadlineMs}ms: ${body}\n${b.stderr()}`);
    }
    if (now > deadline) throw new Error(`the boot was still starting after ${timeoutMs}ms: ${body}\n${b.stderr()}`);
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
}

/** stopHeadless stops the backend as a signal does and waits for it to exit. */
export async function stopHeadless(b: HeadlessBackend, timeoutMs = 120_000): Promise<void> {
  if (b.child.exitCode !== null || b.child.signalCode !== null) return;
  b.child.kill('SIGTERM');
  const code = await exited(b.child, timeoutMs);
  if (code === undefined) throw new Error(`the backend did not stop in ${timeoutMs}ms after SIGTERM\n${b.stderr()}`);
}
