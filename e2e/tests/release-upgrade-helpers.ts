// Helpers for the release upgrade check (release-upgrade.manual.spec.ts):
// the sandboxed environment every process runs in, the launcher's update
// commands and gated headless boot driven as the Windows launcher drives
// them, a read-only census of the database the check compares before and
// after the migration, a deterministic synthetic corpus shaped like a
// year of real use, and the sidebar and timeline reads the migrated boot
// must answer.
import { execFile, type ChildProcess } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { chmod, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { createInterface } from 'node:readline';
import { promisify } from 'node:util';
import { expect, type Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { spawnContained } from '../src/harness-process.js';
import { FALLBACK_MEMORY_LIMIT_BYTES } from '../src/harness-watchdog.js';

const run = promisify(execFile);
export const repoRoot = path.resolve(import.meta.dirname, '..', '..');
/** The candidate: the binary `make harness-build` wrote. */
export const candidateBinary = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
const UPDATE_PREFIX = '__AO_UPDATE__:';
const BOOTSTRAP_PREFIX = '__AO_BOOTSTRAP__:';

/** A sibling of the candidate binary (ao-harness, ao-mockprovider). */
export function candidateSibling(name: string): string {
  return path.join(path.dirname(candidateBinary), name);
}

/** The version `make harness-build` stamps into the candidate. */
export async function buildVersion(): Promise<string> {
  const config = await readFile(path.join(repoRoot, 'build', 'config.yml'), 'utf8');
  const match = /^ {2}version: "(.+)"$/m.exec(config);
  if (!match) throw new Error('build/config.yml names no version');
  return match[1];
}

/** Wall-clock milliseconds of one phase, kept for the report. */
export async function timed<T>(fn: () => Promise<T>): Promise<{ value: T; ms: number }> {
  const started = Date.now();
  const value = await fn();
  return { value, ms: Date.now() - started };
}

export interface Rig {
  /** Everything the check writes lives under here. */
  root: string;
  /** The data root both releases boot on (`--data-dir`). */
  dataRoot: string;
  /** `<dataRoot>/agent-overflow/agent-overflow.db`. */
  dbPath: string;
  /** The from-nothing environment every backend process runs with. */
  env: NodeJS.ProcessEnv;
  /** Where the fake provider and forge CLIs record their calls. */
  calls: string;
}

// A data root, a home and fake provider and forge CLIs, built from nothing
// so no provider home override, WSL interop variable or user PATH entry
// reaches a backend. The login-shell PATH probe runs a stand-in shell that
// skips login files, where a system profile could put a real provider CLI
// ahead of the fakes.
export async function prepareRig(parent: string | undefined): Promise<Rig> {
  if (parent) await mkdir(parent, { recursive: true });
  const root = await mkdtemp(path.join(parent ?? tmpdir(), 'ao-release-upgrade-'));
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
export async function cliCalls(rig: Rig): Promise<string[]> {
  return (await readFile(rig.calls, 'utf8').catch(() => '')).split('\n').filter(Boolean);
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
}

/**
 * runUpdateCommand runs one update command of the candidate to its end,
 * as wsllauncher.UpdateCommandRunner does, and returns its reports.
 */
export async function runUpdateCommand(
  rig: Rig,
  args: string[],
  timeoutMs: number,
): Promise<{ events: UpdateEvent[]; stderr: string }> {
  const child = spawnContained(candidateBinary, [...args, '--data-dir', rig.dataRoot], {
    env: rig.env,
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
 * startHeadless boots the candidate the way the Windows launcher starts a
 * backend after an update: the launcher's own flags, the migration gate,
 * a loopback-only listener.
 */
export async function startHeadless(rig: Rig, bootstrapTimeoutMs: number): Promise<HeadlessBackend> {
  const child = spawnContained(candidateBinary, ['--print-url-fd', '0', '--data-dir', rig.dataRoot, '--listen', '127.0.0.1:0',
    '--refuse-pending-migrations'], {
    env: rig.env,
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
  body: string;
}

/**
 * settledBootstrap polls /bootstrap.json until the boot stops reporting
 * progress, as ProbeBootstrap does, and returns that answer. A boot on a
 * large database spends its starting phase in the boot-time checkpoint
 * and sweeps, so the deadline is the caller's.
 */
export async function settledBootstrap(b: HeadlessBackend, timeoutMs: number): Promise<SettledBootstrap> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const resp = await fetch(`http://127.0.0.1:${b.port}/bootstrap.json`, {
      headers: { authorization: `Bearer ${b.token}` },
    });
    const body = await resp.text();
    if (resp.status !== 503) return { status: resp.status, body };
    if (Date.now() > deadline) throw new Error(`the boot was still starting after ${timeoutMs}ms: ${body}\n${b.stderr()}`);
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

/** One read-only query through `ao-harness db`, rows as objects. */
export async function dbQuery<T>(dbPath: string, sql: string): Promise<T[]> {
  const { stdout } = await run(candidateSibling('ao-harness'), ['-o', 'json', 'db', '--file', dbPath, sql], {
    timeout: 10 * 60_000,
    maxBuffer: 256 * 1024 * 1024,
  });
  return JSON.parse(stdout) as T[];
}

/**
 * Census is what the check compares across the migration: the entity
 * counts a user would notice missing, every thread's top-level item count,
 * and every table's row count so the report can name the tables the corpus
 * leaves empty.
 */
export interface Census {
  schemaVersion: number;
  /** Non-archived projects, what ListProjects and the sidebar show. */
  projects: number;
  archivedProjects: number;
  threads: number;
  archived: number;
  /** Pinned non-archived threads, what HarnessListThreadRows carries. */
  pinned: number;
  groups: number;
  turns: number;
  items: number;
  childItems: number;
  payloads: number;
  payloadBytes: number;
  /** Streaming append chunks beside payloads (payload_chunks). */
  chunks: number;
  chunkBytes: number;
  /** Thread id to its top-level item count, archived threads included. */
  threadItems: Record<string, number>;
  /** Every table in sqlite_master with its row count. */
  tables: Record<string, number>;
}

export async function census(dbPath: string): Promise<Census> {
  const [counts] = await dbQuery<Omit<Census, 'threadItems' | 'tables'>>(dbPath, `
    SELECT (SELECT MAX(version) FROM migration_versions) AS schemaVersion,
           (SELECT COUNT(*) FROM projects WHERE archived = 0) AS projects,
           (SELECT COUNT(*) FROM projects WHERE archived = 1) AS archivedProjects,
           (SELECT COUNT(*) FROM threads WHERE archived = 0) AS threads,
           (SELECT COUNT(*) FROM threads WHERE archived = 1) AS archived,
           (SELECT COUNT(*) FROM threads WHERE archived = 0 AND pinned_at IS NOT NULL) AS pinned,
           (SELECT COUNT(*) FROM thread_groups) AS groups,
           (SELECT COUNT(*) FROM turns) AS turns,
           (SELECT COUNT(*) FROM items) AS items,
           (SELECT COUNT(*) FROM items WHERE parent_id <> '') AS childItems,
           (SELECT COUNT(*) FROM payloads) AS payloads,
           (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payloads) AS payloadBytes,
           (SELECT COUNT(*) FROM payload_chunks) AS chunks,
           (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payload_chunks) AS chunkBytes`);
  const perThread = await dbQuery<{ threadId: string; n: number }>(dbPath,
    `SELECT thread_id AS threadId, COUNT(*) AS n FROM items WHERE parent_id = '' GROUP BY thread_id`);
  const names = await dbQuery<{ name: string }>(dbPath,
    `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`);
  const union = names.map(({ name }) => `SELECT '${name}' AS name, COUNT(*) AS n FROM "${name}"`).join(' UNION ALL ');
  const rows = await dbQuery<{ name: string; n: number }>(dbPath, union);
  return {
    ...counts,
    threadItems: Object.fromEntries(perThread.map((row) => [row.threadId, row.n])),
    tables: Object.fromEntries(rows.map((row) => [row.name, row.n])),
  };
}

/**
 * compareCensus is the rule a migration must keep: the entities a user
 * sees (projects, threads, archived, pinned, groups) are exactly what the
 * previous release wrote; rows (turns, items, payloads and their bytes)
 * never shrink, in total or in any thread, and no thread loses its rows,
 * since a repair migration may add rows (a carrier, a fork's own copy of
 * shown history) but nothing may take one away. Returns every count's
 * delta for the report; throws naming the first violation.
 */
export function compareCensus(before: Census, after: Census): Record<string, number> {
  const exact = ['projects', 'archivedProjects', 'threads', 'archived', 'pinned', 'groups'] as const;
  const monotone = ['turns', 'items', 'childItems', 'payloads'] as const;
  const deltas: Record<string, number> = {};
  for (const key of [...exact, ...monotone, 'payloadBytes', 'chunks', 'chunkBytes'] as const) {
    deltas[key] = after[key] - before[key];
  }
  for (const key of exact) {
    if (after[key] !== before[key]) throw new Error(`${key}: ${before[key]} before the migration, ${after[key]} after`);
  }
  for (const key of monotone) {
    if (after[key] < before[key]) throw new Error(`${key} shrank: ${before[key]} before the migration, ${after[key]} after`);
  }
  const bytesBefore = before.payloadBytes + before.chunkBytes;
  const bytesAfter = after.payloadBytes + after.chunkBytes;
  if (bytesAfter < bytesBefore) throw new Error(`payload bytes shrank: ${bytesBefore} before the migration, ${bytesAfter} after`);
  for (const [threadID, n] of Object.entries(before.threadItems)) {
    const now = after.threadItems[threadID];
    if (now === undefined) throw new Error(`thread ${threadID} had ${n} rows before the migration and none after`);
    if (now < n) throw new Error(`thread ${threadID} had ${n} rows before the migration, ${now} after`);
  }
  return deltas;
}

/** The thread with the most top-level items, from a census. */
export async function largestThread(dbPath: string): Promise<{ id: string; title: string; items: number }> {
  const [row] = await dbQuery<{ id: string; title: string; items: number }>(dbPath, `
    SELECT t.id AS id, t.title AS title, COUNT(i.id) AS items
      FROM threads t JOIN items i ON i.thread_id = t.id AND i.parent_id = ''
     WHERE t.archived = 0
     GROUP BY t.id ORDER BY items DESC, t.id LIMIT 1`);
  if (!row) throw new Error('the database has no non-archived thread with items');
  return row;
}

// ---------------------------------------------------------------------------
// The synthetic corpus.
//
// The shape is a real year of Agent Overflow use, measured on a 3 GB
// database in October 2026: 474 threads over 6 projects, 5 archived, 41%
// Codex; items per thread p50 119, p90 1024, p99 26883, max 160710; turns
// per thread p50 1, p99 179, max 619; payload bytes per item p50 640, p90
// 6 KiB, p99 25 KiB, max 1 MiB, 1.8 GB in all. Thread sizes come from
// stratified quantiles of those distributions, so every scale carries the
// same shape and the same scale always generates the same corpus. What the
// shape cannot carry: subagent rows (59% of the real items have a parent,
// which HarnessSeed does not write), workflow runs, forge state and
// transfers; the report lists the tables the corpus leaves empty.
// ---------------------------------------------------------------------------

/** [quantile, value] knots; values interpolate log-linearly between them. */
type Knots = Array<[number, number]>;
const ITEMS_PER_THREAD: Knots = [[0, 2], [0.5, 100], [0.9, 800], [0.99, 20_000], [1, 160_710]];
const TURNS_PER_THREAD: Knots = [[0, 1], [0.5, 1], [0.9, 25], [0.99, 179], [1, 619]];
const PAYLOAD_BYTES: Knots = [[0, 16], [0.5, 640], [0.9, 6144], [0.99, 25_600], [0.999, 65_536], [1, 1_048_576]];
const REFERENCE_THREADS = 474;
const PROJECT_WEIGHTS = [0.35, 0.25, 0.15, 0.12, 0.08, 0.05];
const CODEX_SHARE = 0.41;
const ARCHIVED_EVERY = 95;
const GROUP_NAMES = ['Backlog', 'In review', 'Spikes', 'Follow-ups'];
const PINNED = 3;
const TOOL_NAMES = ['Bash', 'Read', 'Edit', 'Grep', 'Write'];

function quantile(knots: Knots, q: number): number {
  for (let i = 1; i < knots.length; i++) {
    const [q0, v0] = knots[i - 1];
    const [q1, v1] = knots[i];
    if (q <= q1) {
      const t = (q - q0) / (q1 - q0);
      return Math.max(1, Math.round(Math.exp(Math.log(v0) + t * (Math.log(v1) - Math.log(v0)))));
    }
  }
  return knots[knots.length - 1][1];
}

// mulberry32: a small seeded generator, so a corpus is a function of its
// scale and nothing else.
function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** One HarnessSeed item, as the previous release's seed spec reads it. */
interface SeedItem {
  kind: string;
  summary: string;
  toolName?: string;
  payload?: { kind: string; data: string; padAfter: number };
}

interface SeedTurn {
  userText: string;
  items: SeedItem[];
  repeat?: number;
}

export interface ThreadPlan {
  /** Unique, and its `#n of N` token is what the sidebar search finds. */
  title: string;
  provider: 'claude' | 'codex';
  archived: boolean;
  turns: SeedTurn[];
  /** Top-level rows the seed writes: every turn's user row plus its items. */
  items: number;
  payloadBytes: number;
}

export interface ProjectPlan {
  name: string;
  threads: ThreadPlan[];
  /** Group name to the indexes (into threads) it holds. */
  groups: Record<string, number[]>;
  /** Indexes (into threads) to pin. */
  pinned: number[];
}

export interface CorpusPlan {
  scale: number;
  projects: ProjectPlan[];
  threads: number;
  archived: number;
  groups: number;
  pinned: number;
  items: number;
  payloadBytes: number;
}

/** corpusPlan lays out the corpus at `scale` times the reference thread count. */
export function corpusPlan(scale: number): CorpusPlan {
  if (!(scale > 0)) throw new Error(`corpus scale must be positive, got ${scale}`);
  const total = Math.max(1, Math.round(REFERENCE_THREADS * scale));
  const projectCount = Math.min(PROJECT_WEIGHTS.length, total);
  const random = prng(Math.round(scale * 1_000_003));
  // Thread n takes the (n+0.5)/total quantile of every per-thread
  // distribution, then the order is shuffled so each project gets a mix.
  const order = Array.from({ length: total }, (_, n) => n);
  for (let i = order.length - 1; i > 0; i--) {
    const j = Math.floor(random() * (i + 1));
    [order[i], order[j]] = [order[j], order[i]];
  }
  const projects: ProjectPlan[] = Array.from({ length: projectCount }, (_, p) => ({
    name: `corpus-project-${p + 1}`,
    threads: [],
    groups: {},
    pinned: [],
  }));
  const weightSum = PROJECT_WEIGHTS.slice(0, projectCount).reduce((a, b) => a + b, 0);
  let cursor = 0;
  let archived = 0;
  for (let p = 0; p < projectCount; p++) {
    const share = p === projectCount - 1 ? total - cursor : Math.round((PROJECT_WEIGHTS[p] / weightSum) * total);
    for (const n of order.slice(cursor, cursor + share)) {
      const isArchived = total >= 2 && n % ARCHIVED_EVERY === 1;
      projects[p].threads.push(threadPlan(n, total, isArchived, prng(n * 7919 + 17)));
      if (isArchived) archived++;
    }
    cursor += share;
  }
  // Groups in the first two projects and pins in the first: the sidebar
  // structure a real database carries.
  let groups = 0;
  GROUP_NAMES.forEach((name, g) => {
    const project = projects[g % Math.min(2, projectCount)];
    const members = project.threads
      .map((thread, index) => ({ thread, index }))
      .filter(({ thread, index }) => !thread.archived && index % GROUP_NAMES.length === g)
      .slice(0, 3)
      .map(({ index }) => index);
    if (members.length === 0) return;
    project.groups[name] = members;
    groups++;
  });
  projects[0].pinned = projects[0].threads
    .map((thread, index) => ({ thread, index }))
    .filter(({ thread }) => !thread.archived)
    .slice(0, PINNED)
    .map(({ index }) => index);
  const all = projects.flatMap((project) => project.threads);
  return {
    scale,
    projects,
    threads: total,
    archived,
    groups,
    pinned: projects[0].pinned.length,
    items: all.reduce((sum, thread) => sum + thread.items, 0),
    payloadBytes: all.reduce((sum, thread) => sum + thread.payloadBytes, 0),
  };
}

function threadPlan(n: number, total: number, archived: boolean, random: () => number): ThreadPlan {
  const q = (n + 0.5) / total;
  const items = quantile(ITEMS_PER_THREAD, q);
  const turnCount = Math.min(quantile(TURNS_PER_THREAD, q), items);
  const perTurn = Math.max(1, Math.round(items / turnCount));
  // Three turn shapes (half, whole and one-and-a-half the mean) stand in
  // for the spread of real turns; `repeat` seeds each shape its share of
  // the turns without one spec row per item on the wire.
  const shapes = turnCount >= 3
    ? [Math.max(1, Math.round(perTurn / 2)), perTurn, Math.round(perTurn * 1.5)]
    : [perTurn];
  const repeats = turnCount >= 3
    ? [Math.floor(turnCount / 3), turnCount - 2 * Math.floor(turnCount / 3), Math.floor(turnCount / 3)]
    : [turnCount];
  const turns: SeedTurn[] = [];
  let itemCount = 0;
  let payloadBytes = 0;
  shapes.forEach((k, s) => {
    const turnItems: SeedItem[] = [];
    let turnBytes = 0;
    for (let j = 0; j < k; j++) {
      const item = seedItem(n, s, j, j === k - 1, random);
      turnItems.push(item);
      turnBytes += item.payload?.padAfter ?? 0;
    }
    turns.push({
      userText: `Turn shape ${s + 1} of corpus thread #${n + 1}: please continue with the next step of the task.`,
      items: turnItems,
      ...(repeats[s] > 1 ? { repeat: repeats[s] } : {}),
    });
    itemCount += repeats[s] * (k + 1);
    payloadBytes += repeats[s] * turnBytes;
  });
  const provider = random() < CODEX_SHARE ? 'codex' : 'claude';
  return {
    title: `Corpus thread #${n + 1} of ${total}: ${provider} ${archived ? 'archived' : 'work'} with ${itemCount} rows`,
    provider,
    archived,
    turns,
    items: itemCount,
    payloadBytes,
  };
}

function seedItem(n: number, shape: number, j: number, last: boolean, random: () => number): SeedItem {
  const bytes = quantile(PAYLOAD_BYTES, random());
  const label = `thread ${n + 1} shape ${shape + 1} item ${j + 1}`;
  if (last) {
    return {
      kind: 'assistant_text',
      summary: `Done with ${label}: the change is in and the checks pass.`,
      payload: { kind: 'assistant_text', data: `Done with ${label}.\n`, padAfter: bytes },
    };
  }
  const roll = random();
  if (roll < 0.35) {
    return {
      kind: 'thinking',
      summary: `Thinking through ${label}`,
      payload: { kind: 'thinking', data: `Thinking through ${label}.\n`, padAfter: bytes },
    };
  }
  const toolName = TOOL_NAMES[Math.floor(random() * TOOL_NAMES.length)];
  return {
    kind: 'tool_call',
    toolName,
    summary: `${toolName} for ${label}`,
    payload: {
      kind: toolName === 'Bash' ? 'command_output' : 'tool_call_result',
      data: `${toolName} output for ${label}\n`,
      padAfter: bytes,
    },
  };
}

/** The wire shapes of the previous release's seed result and sidebar rows. */
interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

export interface SeededCorpus {
  /** Milliseconds per project seed, in plan order. */
  projectSeedMs: number[];
}

/**
 * seedCorpus writes the plan through the previous release's HarnessSeed,
 * one call per project (a project's path is unique, so its threads cannot
 * arrive in several calls), then groups and pins threads through the same
 * App methods the sidebar uses.
 */
export async function seedCorpus(previous: HarnessApp, plan: CorpusPlan, seedTimeoutMs: number): Promise<SeededCorpus> {
  const projectSeedMs: number[] = [];
  for (const project of plan.projects) {
    const spec = {
      projects: [{
        name: project.name,
        repo: { commits: [{ message: 'init', files: { 'README.md': `# ${project.name}\n` } }] },
        threads: project.threads.map((thread) => ({
          title: thread.title,
          provider: thread.provider,
          ...(thread.archived ? { archived: true } : {}),
          turns: thread.turns,
        })),
      }],
    };
    const { value: result, ms } = await timed(() => previous.rpcWithin<SeedResult>(seedTimeoutMs, 'HarnessSeed', spec));
    projectSeedMs.push(ms);
    const created = result.projects[0];
    if (created.threadIds.length !== project.threads.length) {
      throw new Error(`${project.name}: seeded ${created.threadIds.length} threads, planned ${project.threads.length}`);
    }
    for (const [name, members] of Object.entries(project.groups)) {
      const group = await previous.rpc<{ id: string }>('CreateThreadGroup', created.projectId, name);
      await previous.rpc('SetThreadGroup', members.map((index) => created.threadIds[index]), group.id);
    }
    for (const index of project.pinned) {
      await previous.rpc('PinThread', created.threadIds[index]);
    }
  }
  return { projectSeedMs };
}

/** HarnessStoreStatus on the wire. */
export interface StoreStatus {
  schemaVersion: number;
  buildSchemaVersion: number;
  deferredWatermark: number;
  deferredLatest: number;
  deferredPending: boolean;
  deferredFailure: { version: number; title: string; failures: number; firstError: string } | null;
}

/**
 * awaitDeferredSettled polls the candidate until its deferred migration
 * phases have finished, and fails on the first recorded failure: a phase
 * that skipped an item is a migration defect, not a slow one.
 */
export async function awaitDeferredSettled(candidate: HarnessApp, timeoutMs: number): Promise<StoreStatus> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const status = await candidate.rpc<StoreStatus>('HarnessStoreStatus');
    if (status.deferredFailure) {
      throw new Error(`deferred migration v${status.deferredFailure.version} (${status.deferredFailure.title}) failed ` +
        `${status.deferredFailure.failures} time(s): ${status.deferredFailure.firstError}`);
    }
    if (!status.deferredPending) return status;
    if (Date.now() > deadline) {
      throw new Error(`deferred migrations still pending after ${timeoutMs}ms: watermark ${status.deferredWatermark}, latest ${status.deferredLatest}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 2_000));
  }
}

/**
 * openThreadByTitle finds a thread through the sidebar search, opens it,
 * and returns how long the timeline took to show its first row. The
 * `#n of N` token of a corpus title is unique; a real database's titles
 * are whatever the user named them, so the caller passes the whole title.
 */
export async function openThreadByTitle(page: Page, title: string, token: string, timeoutMs: number): Promise<number> {
  await page.getByTestId('sidebar-thread-search').fill(token);
  const row = page.getByTestId('thread-row').filter({ hasText: token }).first();
  await expect(row).toBeVisible({ timeout: timeoutMs });
  const started = Date.now();
  await row.click();
  await expect(page.getByTestId('message-timeline-node').first()).toBeVisible({ timeout: timeoutMs });
  const ms = Date.now() - started;
  await expect(page.getByTestId('chat-header')).toContainText(title.slice(0, 40), { timeout: timeoutMs });
  await page.getByTestId('sidebar-thread-search').fill('');
  return ms;
}
