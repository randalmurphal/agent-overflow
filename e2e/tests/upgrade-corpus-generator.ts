// The synthetic corpus: a deterministic function of its scale, seeded
// through a release's HarnessSeed so the database is at that release's
// schema by construction.
import type { HarnessApp } from '../src/harness.js';
import { timed } from './upgrade-corpus-tools.js';

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
// seed cannot write (subagent rows, 59% of the real items; workflow runs;
// attachments; forks), the top-up writes through the candidate's live
// paths (upgrade-corpus-topup.ts).
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

