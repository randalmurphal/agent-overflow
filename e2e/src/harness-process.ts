import { execFile as execFileCallback, spawn, type ChildProcess, type SpawnOptions } from 'node:child_process';
import { readFile, readdir } from 'node:fs/promises';
import { promisify } from 'node:util';
import * as path from 'node:path';

const execFile = promisify(execFileCallback);
export interface ProcessIdentity {
  pid: number;
  birth: string;
  executable: string;
  groupId?: number;
}

export type ProcessGroupMemberProof = ProcessIdentity;

export interface ProcessTreeProof {
  root: ProcessIdentity;
  descendants: ProcessIdentity[];
}

interface ProcessRow {
  pid: number;
  ppid: number;
  rssBytes: number;
  birth?: string;
  executable?: string;
  groupId?: number;
}

/** Spawn the backend with a private process group and platform containment. */
export function spawnContained(
  binary: string,
  args: string[],
  options: SpawnOptions & { memoryLimitBytes: number },
): ChildProcess {
  const { memoryLimitBytes, ...spawnOptions } = options;
  if (process.platform === 'win32') {
    // The Go launcher uses a Job Object on Windows. This direct TS entrypoint
    // cannot create one without a native helper, so the process-tree watchdog
    // still fails visibly on its first unverifiable sample.
    return spawn(binary, args, spawnOptions);
  }
  const kib = Math.floor(memoryLimitBytes / 1024);
  if (!Number.isSafeInteger(kib) || kib < 1) {
    throw new Error('harness memory limit must be at least 1 KiB');
  }
  if (process.platform === 'darwin') {
    // Current macOS kernels reject lowering RLIMIT_DATA, RLIMIT_RSS, and
    // RLIMIT_AS. HarnessWatchdog applies the same exact-tree ceiling at 100ms
    // cadence, and the outer ao-harness-e2e launcher monitors the whole suite.
    return spawn(binary, args, spawnOptions);
  }
  // RLIMIT_AS (`ulimit -v`) prevents Go and Node from reserving their normal
  // runtime address space before the watchdog can start. RLIMIT_DATA bounds
  // heap/data allocations without blocking those virtual reservations.
  const script = 'limit="$1"; shift; ulimit -d "$limit" || exit 125; exec "$@"';
  return spawn(
    '/bin/sh',
    ['-c', script, 'ao-harness-memory-limit', String(kib), binary, ...args],
    spawnOptions,
  );
}

export async function captureProcessIdentity(pid: number | undefined): Promise<ProcessIdentity> {
  if (!pid || pid <= 0) throw new Error('harness watchdog: child has no pid');
  // Linux fallback containment starts through `/bin/sh` so it can apply
  // ulimit before exec'ing the backend. Do not authenticate that short-lived
  // wrapper. A later identity check would otherwise reject the same PID when
  // exec turns it into the backend process.
  let previous: ProcessIdentity | undefined;
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const identity = await captureProcessIdentityOnce(pid);
    if (!isShellExecutable(identity.executable) && previous && sameProcessIdentity(previous, identity)) {
      return identity;
    }
    previous = isShellExecutable(identity.executable) ? undefined : identity;
    if (attempt === 99) return identity;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error(`harness watchdog: could not observe exec'd process ${pid}`);
}

function sameProcessIdentity(left: ProcessIdentity, right: ProcessIdentity): boolean {
  if (left.pid !== right.pid || left.birth !== right.birth || left.groupId !== right.groupId) {
    return false;
  }
  // Darwin's comm changes across a shebang interpreter exec even though the
  // PID, kernel birth marker, and owned process group remain the same. Treat
  // that legitimate same-process exec as stable; PID reuse changes birth.
  return process.platform === 'darwin'
    || path.resolve(left.executable) === path.resolve(right.executable);
}

async function captureProcessIdentityOnce(pid: number): Promise<ProcessIdentity> {
  if (process.platform === 'linux') {
    const stat = await readFile(`/proc/${pid}/stat`, 'utf8');
    const close = stat.lastIndexOf(')');
    const fields = stat.slice(close + 1).trim().split(/\s+/);
    if (close < 0 || fields.length < 20) {
      throw new Error(`harness watchdog: incomplete /proc/${pid}/stat`);
    }
    const { stdout: executable } = await execFile('/usr/bin/readlink', [`/proc/${pid}/exe`]);
    return { pid, birth: fields[19], executable: executable.trim() };
  }
  if (process.platform === 'darwin') {
    const [{ stdout: birth }, { stdout: executable }, { stdout: group }] = await Promise.all([
      execFile('/bin/ps', ['-o', 'lstart=', '-p', String(pid)]),
      execFile('/bin/ps', ['-o', 'comm=', '-p', String(pid)]),
      execFile('/bin/ps', ['-o', 'pgid=', '-p', String(pid)]),
    ]);
    if (!birth.trim() || !executable.trim() || !group.trim()) {
      throw new Error(`harness watchdog: process ${pid} is gone`);
    }
    return { pid, birth: birth.trim(), executable: executable.trim(), groupId: Number(group.trim()) };
  }
  if (process.platform === 'win32') {
    const script =
      `$p=Get-CimInstance Win32_Process -Filter \"ProcessId=${pid}\"; ` +
      'if ($null -eq $p) { exit 3 }; ' +
      '$p | Select-Object ProcessId,CreationDate,ExecutablePath | ConvertTo-Json -Compress';
    const { stdout } = await execFile('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-Command',
      script,
    ]);
    const row = JSON.parse(stdout) as {
      ProcessId: number;
      CreationDate: string;
      ExecutablePath: string;
    };
    if (row.ProcessId !== pid || !row.CreationDate || !row.ExecutablePath) {
      throw new Error(`harness watchdog: incomplete Windows identity for ${pid}`);
    }
    return { pid, birth: row.CreationDate, executable: row.ExecutablePath };
  }
  throw new Error(`harness watchdog: unsupported platform ${process.platform}`);
}

function isShellExecutable(executable: string): boolean {
  const name = path.basename(executable).toLowerCase();
  return name === 'sh' || name === 'dash' || name === 'bash' || name === 'zsh';
}

export async function verifyProcessIdentity(identity: ProcessIdentity): Promise<boolean> {
  try {
    const current = await captureProcessIdentity(identity.pid);
    return sameProcessIdentity(identity, current);
  } catch {
    return false;
  }
}

export async function captureProcessGroupMemberProof(
  identity: ProcessIdentity,
): Promise<ProcessGroupMemberProof | undefined> {
  if (identity.groupId === undefined) return undefined;
  return captureProcessGroupMemberProofForGroup(identity.pid, identity.groupId);
}

export async function captureProcessGroupMemberProofForPID(
  pid: number | undefined,
): Promise<ProcessGroupMemberProof | undefined> {
  if (!pid || pid <= 0 || (process.platform !== 'linux' && process.platform !== 'darwin')) return undefined;
  const root = (await processRows()).find((row) => row.pid === pid);
  if (!root || root.groupId === undefined) return undefined;
  return captureProcessGroupMemberProofForGroup(pid, root.groupId);
}

async function captureProcessGroupMemberProofForGroup(
  rootPID: number,
  groupId: number,
): Promise<ProcessGroupMemberProof | undefined> {
  const member = (await processRows()).find(
    (row) => row.groupId === groupId && row.pid !== rootPID && row.birth && row.executable,
  );
  if (!member || !member.birth || !member.executable) return undefined;
  return { pid: member.pid, birth: member.birth, executable: member.executable, groupId: member.groupId };
}

export async function verifyProcessGroupMemberProof(
  proof: ProcessGroupMemberProof,
): Promise<boolean> {
  try {
    const current = await captureProcessIdentity(proof.pid);
    return sameProcessIdentity(proof, current);
  } catch {
    return false;
  }
}

export async function captureProcessTreeProof(identity: ProcessIdentity): Promise<ProcessTreeProof> {
  const rows = await processRows();
  const children = new Map<number, ProcessRow[]>();
  for (const row of rows) {
    const list = children.get(row.ppid) ?? [];
    list.push(row);
    children.set(row.ppid, list);
  }
  const descendants: ProcessIdentity[] = [];
  const queue = [identity.pid];
  const seen = new Set<number>();
  while (queue.length) {
    const pid = queue.shift()!;
    if (seen.has(pid)) continue;
    seen.add(pid);
    for (const child of children.get(pid) ?? []) {
      queue.push(child.pid);
      if (!child.birth || !child.executable) continue;
      descendants.push({ pid: child.pid, birth: child.birth, executable: child.executable, groupId: child.groupId });
    }
  }
  return { root: identity, descendants };
}

export async function processTreeRSS(identity: ProcessIdentity): Promise<number> {
  const rows = await processRows();
  const byPid = new Map(rows.map((row) => [row.pid, row]));
  const root = byPid.get(identity.pid);
  if (!root || root.birth !== identity.birth) {
    throw new Error(`harness watchdog: process identity changed for pid ${identity.pid}`);
  }
  const children = new Map<number, ProcessRow[]>();
  for (const row of rows) {
    const list = children.get(row.ppid) ?? [];
    list.push(row);
    children.set(row.ppid, list);
  }
  let total = 0;
  const queue = [identity.pid];
  const seen = new Set<number>();
  while (queue.length) {
    const pid = queue.shift()!;
    if (seen.has(pid)) continue;
    seen.add(pid);
    const row = byPid.get(pid);
    if (row) total += row.rssBytes;
    for (const child of children.get(pid) ?? []) queue.push(child.pid);
  }
  return total;
}

async function processRows(): Promise<ProcessRow[]> {
  if (process.platform === 'linux') {
    const entries = await readdir('/proc', { withFileTypes: true });
    const rows: ProcessRow[] = [];
    for (const entry of entries) {
      if (!entry.isDirectory() || !/^\d+$/.test(entry.name)) continue;
      const pid = Number(entry.name);
      try {
        const stat = await readFile(`/proc/${pid}/stat`, 'utf8');
        const close = stat.lastIndexOf(')');
        const fields = stat.slice(close + 1).trim().split(/\s+/);
        const status = await readFile(`/proc/${pid}/status`, 'utf8');
        const rss = /^VmRSS:\s+(\d+)\s+kB$/m.exec(status);
        if (close < 0 || fields.length < 20 || !rss) continue;
        rows.push({
          pid,
          ppid: Number(fields[1]),
          birth: fields[19],
          groupId: Number(fields[2]),
          rssBytes: Number(rss[1]) * 1024,
        });
      } catch {
        // Processes can disappear between the directory read and each file.
      }
    }
    return rows;
  }
  if (process.platform === 'darwin') {
    const { stdout } = await execFile('/bin/ps', ['-axo', 'pid=,ppid=,pgid=,rss=,lstart=,comm=']);
    return stdout.split('\n').flatMap((line) => {
      const fields = line.trim().split(/\s+/);
      // Four numeric columns + five lstart fields + comm. A command without
      // spaces has exactly ten fields; requiring eleven drops every ordinary
      // process and makes the watchdog report a false identity change.
      if (fields.length < 10) return [];
      const pid = Number(fields[0]);
      const ppid = Number(fields[1]);
      const groupId = Number(fields[2]);
      const rss = Number(fields[3]);
      const birth = fields.slice(4, 9).join(' ');
      const executable = fields.slice(9).join(' ');
      return Number.isInteger(pid) && Number.isInteger(ppid) && Number.isInteger(groupId) && Number.isFinite(rss)
        ? [{ pid, ppid, birth, executable, groupId, rssBytes: rss * 1024 }]
        : [];
    });
  }
  if (process.platform === 'win32') {
    const script =
      'Get-CimInstance Win32_Process | ' +
      'Select-Object ProcessId,ParentProcessId,WorkingSetSize,CreationDate,ExecutablePath | ' +
      'ConvertTo-Json -Compress';
    const { stdout } = await execFile('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-Command',
      script,
    ]);
    const parsed = JSON.parse(stdout) as
      | Array<{
          ProcessId: number;
          ParentProcessId: number;
          WorkingSetSize: number;
          CreationDate: string;
          ExecutablePath: string;
        }>
      | {
          ProcessId: number;
          ParentProcessId: number;
          WorkingSetSize: number;
          CreationDate: string;
          ExecutablePath: string;
        };
    const values = Array.isArray(parsed) ? parsed : [parsed];
    return values
      .filter((row) => row.ExecutablePath)
      .map((row) => ({
        pid: row.ProcessId,
        ppid: row.ParentProcessId,
        rssBytes: row.WorkingSetSize,
        birth: row.CreationDate,
        executable: row.ExecutablePath,
      }));
  }
  throw new Error(`harness watchdog: unsupported platform ${process.platform}`);
}

export async function availableMemoryBytes(): Promise<number> {
  if (process.platform === 'linux') {
    const meminfo = await readFile('/proc/meminfo', 'utf8');
    const match = /^MemAvailable:\s+(\d+)\s+kB$/m.exec(meminfo);
    if (!match) throw new Error('harness watchdog: /proc/meminfo has no MemAvailable');
    return Number(match[1]) * 1024;
  }
  if (process.platform === 'darwin') {
    const { stdout } = await execFile('/usr/bin/vm_stat');
    const size = /page size of (\d+) bytes/.exec(stdout)?.[1];
    const pageSize = Number(size ?? 4096);
    let pages = 0;
    for (const name of ['Pages free', 'Pages inactive', 'Pages speculative', 'Pages purgeable']) {
      const match = new RegExp(`^${name}:\\s+(\\d+)`, 'm').exec(stdout);
      if (match) pages += Number(match[1]);
    }
    if (!pages) throw new Error('harness watchdog: vm_stat has no available pages');
    return pages * pageSize;
  }
  if (process.platform === 'win32') {
    const { stdout } = await execFile('powershell.exe', [
      '-NoProfile',
      '-NonInteractive',
      '-Command',
      '(Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory',
    ]);
    return Number(stdout.trim()) * 1024;
  }
  throw new Error(`harness watchdog: unsupported platform ${process.platform}`);
}

/** Signal only the group created by launchHarness, never the test runner. */
async function terminateChildTree(child: ChildProcess, signal: NodeJS.Signals): Promise<void> {
  const pid = child.pid;
  if (!pid) return;
  if (process.platform === 'win32') throw new Error('Windows teardown requires a complete process-tree proof');
  try {
    process.kill(-pid, signal);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== 'ESRCH') throw err;
  }
}

export async function waitForOwnedTreeExit(
  child: ChildProcess,
  timeoutMs: number,
): Promise<{ resolved: boolean }> {
  const pid = child.pid;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const childExited = child.exitCode !== null || child.signalCode !== null;
    const groupAlive = process.platform !== 'win32' && pid ? unixProcessGroupAlive(pid) : false;
    if (childExited && !groupAlive) return { resolved: true };
    if (Date.now() >= deadline) return { resolved: false };
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

/**
 * Recheck only identities captured from this launch. A Unix process-group
 * probe can return EPERM after the owned members are gone (notably on macOS),
 * and treating that ambiguous kernel answer as proof of survival both lies in
 * diagnostics and preserves a disposable data root forever.
 */
export async function ownedProcessTreeAlive(
  child: ChildProcess,
  identity?: ProcessIdentity,
  memberProof?: ProcessGroupMemberProof,
  treeProof?: ProcessTreeProof,
): Promise<boolean> {
  const proofs = [identity, memberProof, treeProof?.root, ...(treeProof?.descendants ?? [])]
    .filter((proof): proof is ProcessIdentity => proof !== undefined);
  const seen = new Set<string>();
  for (const proof of proofs) {
    const key = `${proof.pid}:${proof.birth}`;
    if (seen.has(key)) continue;
    seen.add(key);
    if (await verifyProcessIdentity(proof)) return true;
  }
  // When launch failed before any identity could be captured, absence of a
  // proof is not permission to declare the process gone. The ChildProcess
  // status is the only safe evidence left.
  return proofs.length === 0 && child.exitCode === null && child.signalCode === null;
}

export async function terminateChildTreeAndWaitVerified(
  child: ChildProcess,
  identity: ProcessIdentity | undefined,
  signal: NodeJS.Signals,
  memberProof?: ProcessGroupMemberProof,
  treeProof?: ProcessTreeProof,
): Promise<void> {
  // A failed boot can exit before cleanup enters this function. There is no
  // PID to signal in that case, and replacing the original boot error with an
  // identity error makes the failure both misleading and harder to diagnose.
  if (child.exitCode !== null || child.signalCode !== null) {
    if (process.platform === 'win32') {
      if (!treeProof || !(await terminateWindowsTree(treeProof, signal))) {
        throw new Error(`refusing to release Windows harness root ${child.pid}: no complete tree proof`);
      }
      return;
    }
    if (!child.pid || !unixProcessGroupAlive(child.pid)) return;
    if (!memberProof || !(await verifyProcessGroupMemberProof(memberProof))) {
      throw new Error(`refusing to signal group for dead pid ${child.pid}: no matching member proof`);
    }
    // The group remains the ownership boundary after its leader exits. The
    // surviving member proof prevents a recycled group from being signalled.
    await terminateChildTree(child, signal);
    const exited = await waitForOwnedTreeExit(child, signal === 'SIGKILL' ? 5_000 : 500);
    if (exited.resolved) return;
    await terminateChildTree(child, 'SIGKILL');
    const forced = await waitForOwnedTreeExit(child, 5_000);
    if (!forced.resolved) throw new Error(`harness process group ${child.pid} survived forced termination`);
    return;
  }
  if (process.platform === 'win32') {
    if (!treeProof || !(await terminateWindowsTree(treeProof, signal))) {
      throw new Error(`refusing to signal Windows harness ${child.pid}: no complete tree proof`);
    }
    return;
  }
  if (!identity) {
    if (!memberProof || !(await verifyProcessGroupMemberProof(memberProof))) {
      throw new Error(`refusing to signal pid ${child.pid ?? 'unknown'} without process identity or member proof`);
    }
  } else if (!(await verifyProcessIdentity(identity))) {
    throw new Error(`refusing to signal pid ${child.pid ?? 'unknown'} without matching process identity`);
  }
  await terminateChildTree(child, signal);
  const exited = await waitForOwnedTreeExit(child, signal === 'SIGKILL' ? 5_000 : 500);
  if (exited.resolved) return;
  if (identity) {
    if (!(await verifyProcessIdentity(identity))) {
      // The group leader can exit after the graceful group signal while an
      // owned helper (Chromium is the common case) is still unwinding. The
      // original leader identity cannot authenticate escalation any more, but
      // a previously captured, still-live member identity can. This is the
      // same proven-group case handled above when the leader was already gone
      // on entry; cover the race where it disappears during the grace window.
      if (!memberProof || !(await verifyProcessGroupMemberProof(memberProof))) {
        throw new Error(`refusing escalation because pid ${identity.pid} identity changed`);
      }
    }
  } else if (!memberProof || !(await verifyProcessGroupMemberProof(memberProof))) {
    throw new Error(`refusing escalation because pid ${child.pid ?? 'unknown'} member proof changed`);
  }
  await terminateChildTree(child, 'SIGKILL');
  const forced = await waitForOwnedTreeExit(child, 5_000);
  if (!forced.resolved) {
    throw new Error(
      `harness process ${identity?.pid ?? child.pid ?? 'unknown'} survived verified forced termination`,
    );
  }
}

type IdentityState = 'match' | 'gone' | 'changed';

async function processIdentityState(identity: ProcessIdentity): Promise<IdentityState> {
  try {
    const current = await captureProcessIdentity(identity.pid);
    return current.birth === identity.birth &&
      path.resolve(current.executable) === path.resolve(identity.executable)
      ? 'match'
      : 'changed';
  } catch (error) {
    try {
      process.kill(identity.pid, 0);
    } catch (probeError) {
      if ((probeError as NodeJS.ErrnoException).code === 'ESRCH') return 'gone';
      throw new Error(`harness process ${identity.pid} identity probe failed: ${(error as Error).message}`);
    }
    throw new Error(`harness process ${identity.pid} identity query failed while it is alive: ${(error as Error).message}`);
  }
}

async function terminateWindowsTree(
  proof: ProcessTreeProof,
  signal: NodeJS.Signals,
): Promise<boolean> {
  if (signal !== 'SIGKILL' && signal !== 'SIGTERM') throw new Error(`unsupported Windows teardown signal ${signal}`);
  const members = [proof.root, ...proof.descendants];
  for (const member of members) {
    const state = await processIdentityState(member);
    if (state === 'gone') {
      continue;
    }
    if (state === 'changed') throw new Error(`Windows harness process ${member.pid} identity changed`);
    try {
      process.kill(member.pid, signal);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
    }
  }
  const deadline = Date.now() + 5_000;
  for (;;) {
    const states = await Promise.all(members.map((member) => processIdentityState(member)));
    if (states.every((state) => state === 'gone')) return true;
    if (states.some((state) => state === 'changed')) {
      throw new Error('Windows harness process identity changed during teardown');
    }
    if (Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

function unixProcessGroupAlive(pid: number): boolean {
  try {
    process.kill(-pid, 0);
    return true;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === 'ESRCH') return false;
    if (code === 'EPERM') return true;
    throw error;
  }
}
