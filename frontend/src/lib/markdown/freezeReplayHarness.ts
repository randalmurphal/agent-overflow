// Replay machinery for driving a recorded streaming corpus through the
// production markdown pipeline at controlled chunk boundaries, with a timing
// guard and a timing table.
//
// Why this is a permanent asset rather than throwaway spike code: the
// renderer-freeze class it exists for (2026-08-03 / 2026-08-07 — one core
// pegged, no paint, no error, nothing in any log) is only ever reproducible
// from real streamed content at real chunk boundaries, and the corpus that
// exposes it is a session capture that cannot live in the repo. The corpus is
// disposable; the rig that replays one is not. `freezeReplay.manual.ts` is the
// driver that points this at a capture.
//
// Everything here is content-independent. It mirrors two production shapes and
// must be kept in step with them, because a divergence is a bug in one of the
// two and this rig is what would notice:
//
//   - `ChatMarkdown.svelte`'s committed-prefix / volatile-tail split over
//     `StreamingBoundarySplitter` (`ChatMarkdownPipeline` below), including
//     the per-block `incrementalLex` caches keyed by BLOCK INDEX, which is how
//     `Block.svelte` keys them.
//   - `PerItemSmoother`'s word-unit reveal granularity (`wordUnitOffsets`) —
//     the step size `ChatMarkdown` actually sees, which is far finer than the
//     wire's own chunking.

import {
  parseBlocks,
  createParseBlocksCache,
  incrementalLex,
  createIncrementalLexCache,
  createProvenAppend,
  parseIncompleteMarkdown,
  type ProvenAppend,
} from './index';
import { StreamingBoundarySplitter } from './boundary';

/** Default ceiling for a single replay step. Anything near it is the bug. */
export const DEFAULT_STEP_BUDGET_MS = 2_000;

// ── The pipeline under test ───────────────────────────────────────────────

/**
 * One `<Streamdown>` instance. `Streamdown.svelte` derives
 * `parseBlocks(content, extensions, blocksCache)`, then each `Block.svelte`
 * derives `incrementalLex(block, extensions, lexCache, parseIncompleteMarkdown
 * | null)`. Blocks are keyed by INDEX, so a Block instance — and its lex cache
 * — is reused for whatever string lands at that index, which is exactly the
 * reuse pattern a cache bug hides in.
 */
export class StreamdownInstance {
  private blocksCache = createParseBlocksCache();
  private lexCaches: ReturnType<typeof createIncrementalLexCache>[] = [];
  private lastContent: string | undefined;
  /** `cache.lastPath` per block per render — the incremental fast-path breadcrumb. */
  readonly paths: string[] = [];

  constructor(private readonly completeIncomplete: boolean) {}

  render(content: string, append?: ProvenAppend): number {
    // A mounted Svelte component does not re-run its content-derived parser
    // when its primitive string prop is unchanged. The committed Streamdown
    // instance commonly stays unchanged for thousands of tail reveals.
    if (content === this.lastContent) return 0;
    this.lastContent = content;
    const blocks = parseBlocks(content, [], this.blocksCache, append);
    let tokenCount = 0;
    for (let i = 0; i < blocks.length; i++) {
      if (!this.lexCaches[i]) this.lexCaches[i] = createIncrementalLexCache();
      const cache = this.lexCaches[i];
      const tokens = incrementalLex(
        blocks[i],
        [],
        cache,
        this.completeIncomplete ? parseIncompleteMarkdown : null,
        i === blocks.length - 1 ? this.blocksCache.lastBlockAppend : undefined,
      );
      this.paths.push(cache.lastPath as string);
      tokenCount += tokens.length;
    }
    // Block instances beyond the current count unmount; drop their caches.
    this.lexCaches.length = blocks.length;
    return tokenCount;
  }
}

/** Mirrors `ChatMarkdown.svelte`: splitter + committed prefix + volatile tail. */
export class ChatMarkdownPipeline {
  private splitter = new StreamingBoundarySplitter();
  private prefix = new StreamdownInstance(false);
  private tail = new StreamdownInstance(true);
  private previousSource = '';

  step(source: string, appendDelta?: string): void {
    const append = appendDelta !== undefined &&
      source.length === this.previousSource.length + appendDelta.length
      ? createProvenAppend(this.previousSource, appendDelta)
      : undefined;
    // Replay drivers supply exact append deltas. Use the proof's constructed
    // value so the harness exercises production's no-prefix-scan path.
    const canonicalSource = append?.next ?? source;
    this.previousSource = canonicalSource;
    const { prefix, tail } = this.splitter.split(canonicalSource, append);
    if (prefix) this.prefix.render(prefix);
    if (tail || !prefix) this.tail.render(tail, this.splitter.tailAppend);
  }
}

/** Settled (non-streaming) render: one instance, no speculative completion. */
export function renderSettled(source: string): void {
  new StreamdownInstance(false).render(source);
}

/** A driver factory: builds fresh pipeline state, returns its step function. */
export type ReplayDriver = () => (source: string) => void;

export const streamingDriver: ReplayDriver = () => {
  const pipeline = new ChatMarkdownPipeline();
  let previousLength = 0;
  return (source) => {
    const appendDelta = source.slice(previousLength);
    previousLength = source.length;
    pipeline.step(source, appendDelta);
  };
};

export const settledDriver: ReplayDriver = () => (source) => renderSettled(source);

// ── Step generators ───────────────────────────────────────────────────────

function isWhitespace(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r';
}

/**
 * `PerItemSmoother`'s word-unit granularity — the offsets after each
 * (whitespace run, word, trailing whitespace run) triple, which is the step
 * size `ChatMarkdown` really re-renders at.
 */
export function wordUnitOffsets(text: string): number[] {
  const out: number[] = [];
  let i = 0;
  while (i < text.length) {
    while (i < text.length && isWhitespace(text[i])) i++;
    while (i < text.length && !isWhitespace(text[i])) i++;
    while (i < text.length && isWhitespace(text[i])) i++;
    out.push(i);
  }
  if (out.length === 0 || out[out.length - 1] !== text.length) out.push(text.length);
  return out;
}

/** Deterministic PRNG (mulberry32) so a failing fuzz seed is reproducible. */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Seeded re-chunking of `text`: arbitrary boundaries, always ending at the end. */
export function randomOffsets(text: string, seed: number, maxSteps: number): number[] {
  const rnd = mulberry32(seed);
  const set = new Set<number>([text.length]);
  const n = Math.min(maxSteps, Math.max(1, Math.floor(text.length / 3)));
  for (let i = 0; i < n; i++) {
    set.add(1 + Math.floor(rnd() * Math.max(1, text.length - 1)));
  }
  return Array.from(set).sort((a, b) => a - b);
}

// ── Replay driver ─────────────────────────────────────────────────────────

export interface ReplayStat {
  label: string;
  /** Worst single step, and the prefix length it happened at. */
  maxStepMs: number;
  maxStepAt: number;
  totalMs: number;
  steps: number;
  /** Total characters replayed, or -1 for a one-shot timed probe. */
  chars: number;
}

/**
 * Collects replay timings so a sweep can print one table at the end.
 *
 * The step budget is a TRIPWIRE, not a performance assertion: a genuine
 * non-termination never reaches it (it wedges the worker, which is itself the
 * signal). What it catches is the catastrophically super-linear
 * but-still-terminating shape, which otherwise looks like a slow test run.
 */
export class ReplayRecorder {
  readonly stats: ReplayStat[] = [];

  constructor(readonly stepBudgetMs: number = DEFAULT_STEP_BUDGET_MS) {}

  /** Replay `text` through a fresh driver, stopping at each offset. */
  replay(label: string, text: string, offsets: readonly number[], driver: ReplayDriver): ReplayStat {
    const step = driver();
    let maxStepMs = 0;
    let maxStepAt = -1;
    const t0 = performance.now();
    for (const off of offsets) {
      const s0 = performance.now();
      step(text.slice(0, off));
      const dt = performance.now() - s0;
      if (dt > maxStepMs) {
        maxStepMs = dt;
        maxStepAt = off;
      }
      if (dt > this.stepBudgetMs) {
        throw new Error(
          `[${label}] single step exceeded ${this.stepBudgetMs}ms (${dt.toFixed(0)}ms) at offset ${off}\n` +
            `tail: ${JSON.stringify(text.slice(Math.max(0, off - 200), off))}`,
        );
      }
    }
    const stat: ReplayStat = {
      label,
      maxStepMs,
      maxStepAt,
      totalMs: performance.now() - t0,
      steps: offsets.length,
      chars: text.length,
    };
    this.stats.push(stat);
    return stat;
  }

  /** Time one arbitrary probe into the same table. Returns its duration. */
  record(label: string, fn: () => void): number {
    const t0 = performance.now();
    fn();
    const dt = performance.now() - t0;
    this.stats.push({ label, maxStepMs: dt, maxStepAt: -1, totalMs: dt, steps: 1, chars: -1 });
    return dt;
  }

  /** The worst recorded single step, or null when nothing has been recorded. */
  worst(): ReplayStat | null {
    let worst: ReplayStat | null = null;
    for (const stat of this.stats) {
      if (worst === null || stat.maxStepMs > worst.maxStepMs) worst = stat;
    }
    return worst;
  }

  /** Fixed-width timing table, worst `limit` entries by single-step cost. */
  table(limit = 40): string {
    const sorted = [...this.stats].sort((a, b) => b.maxStepMs - a.maxStepMs).slice(0, limit);
    const lines = sorted.map(
      (s) =>
        `${s.maxStepMs.toFixed(2).padStart(9)}ms max-step @${String(s.maxStepAt).padStart(6)}  ` +
        `${s.totalMs.toFixed(1).padStart(9)}ms total  ${String(s.steps).padStart(5)} steps  ` +
        `${String(s.chars).padStart(6)} chars  ${s.label}`,
    );
    return (
      `=== replay timing profile (${this.stats.length} replays, ` +
      `worst ${sorted.length} by max single step) ===\n${lines.join('\n')}`
    );
  }
}
