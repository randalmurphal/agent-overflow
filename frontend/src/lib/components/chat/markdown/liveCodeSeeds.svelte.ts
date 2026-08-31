// Live syntax-span seeds for streaming code blocks. The backend
// watches its own copy of a streaming assistant_text row for fenced
// code, highlights it, and pushes span metadata on the remote-only
// `highlight:seed` channel (app_highlight.go / internal/highlightapp). A remote client
// colors the streaming block from the seed riding the event stream —
// one-way latency — instead of paying a WAN round trip per growth
// step. Loopback clients never receive these frames.
//
// Seeds carry NO text. Alignment is verified content-addressing: the
// seed's cumulative per-line fnv1a chain (UTF-16 code units, matching
// utils/fnv1a.ts — parity pinned by internal/highlight/jshash_test.go)
// is compared against a running hash of the token text the host
// already holds. The longest matching line prefix is exactly the
// region the seed's spans are proven to describe; any divergence
// (backend fence scanner vs marked, edited content, hash collision at
// worst one render) degrades to the RPC path, never to misaligned
// colors.

import type { EncodedLine } from '../../../utils/syntaxSpans';

/** Wire payload of `highlight:seed` (Go: HighlightSeedEvent). Seeds
 * are complete results only — the producer skips transient parse
 * degradation, so there is no incomplete flag to honor here. */
export interface HighlightSeedEvent {
  threadId: string;
  itemId: string;
  lang: string;
  /** Frontend contentKey(source); final seeds only. */
  contentKey?: string;
  lineHashes: number[] | null;
  lines: EncodedLine[] | null;
  final: boolean;
}

/** One retained seed per (thread, item, fence language): a text row
 * streams at most one open fence, so latest-wins per key tracks the
 * growing block without accumulating superseded pushes — while
 * concurrent rows (subagent fan-out) streaming the same language keep
 * separate slots instead of evicting each other every tick. */
export const MAX_LIVE_CODE_SEEDS = 8;

interface LiveCodeSeed {
  lang: string;
  lineHashes: number[];
  lines: EncodedLine[];
}

// LRU by Map insertion order, keyed `${threadId}|${itemId}|${lang}`.
// Matching (below) scans values by lang — the key only decides which
// pushes replace each other.
const seeds = new Map<string, LiveCodeSeed>();

// Bumped on every put so hosts re-evaluate their match. Read OUTSIDE
// the host effect's untrack block — it is the only signal that a seed
// arrived between token changes (e.g. the final seed after the last
// delta).
let generation = $state(0);

export function liveCodeSeedGeneration(): number {
  return generation;
}

export function putLiveCodeSeed(
  threadId: string,
  itemId: string,
  lang: string,
  lineHashes: number[],
  lines: EncodedLine[],
): void {
  if (!lang || lineHashes.length === 0) return;
  const key = `${threadId}|${itemId}|${lang}`;
  seeds.delete(key);
  seeds.set(key, { lang, lineHashes, lines });
  while (seeds.size > MAX_LIVE_CODE_SEEDS) {
    const oldest = seeds.keys().next().value;
    if (oldest === undefined) break;
    seeds.delete(oldest);
  }
  generation += 1;
}

export interface LiveCodeSeedMatch {
  /** The verified prefix of the queried text (whole lines). */
  covered: string;
  /** Seed spans sliced to exactly the covered lines. */
  spans: EncodedLine[];
  /** True when the seed and the queried text verify as IDENTICAL —
   * every line of the text matched AND the seed's chain is fully
   * consumed. A seed extending past the text is prefix coverage, not
   * exact: its spans were parsed with a suffix this block may never
   * contain, and adopting them as final would cancel the block's own
   * RPC. */
  exact: boolean;
}

/**
 * Finds the seed whose hash chain verifies the longest line prefix of
 * `text`. The host adopts `spans` against `covered` — for an exact
 * match that is the whole text (no RPC needed); for a partial match
 * the existing stale-prefix rendering paints the covered lines while
 * the exact request runs.
 */
export function matchLiveCodeSeed(lang: string, text: string): LiveCodeSeedMatch | null {
  if (!lang || !text) return null;
  let bestSeed: LiveCodeSeed | null = null;
  let bestLines = 0;
  let bestEnd = 0;
  for (const seed of seeds.values()) {
    if (seed.lang !== lang) continue;
    const { matchedLines, endOffset } = matchedLinePrefix(text, seed.lineHashes);
    if (matchedLines > bestLines || (matchedLines === bestLines && endOffset > bestEnd)) {
      bestSeed = seed;
      bestLines = matchedLines;
      bestEnd = endOffset;
    }
  }
  if (!bestSeed || bestLines === 0) return null;
  return {
    covered: text.slice(0, bestEnd),
    spans: bestSeed.lines.slice(0, bestLines),
    exact: bestEnd === text.length && bestLines === bestSeed.lineHashes.length,
  };
}

const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

/**
 * Walks `text` with the same running fnv1a the chain was built from,
 * counting how many leading lines the chain verifies. A mismatch stops
 * the walk — the chain is cumulative, so later entries cannot match a
 * diverged prefix. `endOffset` is the char offset just past the last
 * verified line (excluding its newline).
 */
function matchedLinePrefix(
  text: string,
  chain: number[],
): { matchedLines: number; endOffset: number } {
  let hash = FNV_OFFSET_BASIS_32 >>> 0;
  let line = 0;
  let matchedLines = 0;
  let endOffset = 0;
  for (let i = 0; i < text.length; i += 1) {
    const code = text.charCodeAt(i);
    if (code === 10) {
      if (line >= chain.length || chain[line] !== hash) {
        return { matchedLines, endOffset };
      }
      matchedLines = line + 1;
      endOffset = i;
      line += 1;
    }
    hash = Math.imul(hash ^ code, FNV_PRIME_32) >>> 0;
  }
  if (line < chain.length && chain[line] === hash) {
    matchedLines = line + 1;
    endOffset = text.length;
  }
  return { matchedLines, endOffset };
}

/** The chain the backend sends (highlight.FrontendLineHashes parity):
 * entry i = fnv1a of the first i+1 lines joined by '\n'. Exported for
 * tests that simulate seed pushes. */
export function lineHashChain(text: string): number[] {
  const chain: number[] = [];
  let hash = FNV_OFFSET_BASIS_32 >>> 0;
  for (let i = 0; i < text.length; i += 1) {
    const code = text.charCodeAt(i);
    if (code === 10) chain.push(hash);
    hash = Math.imul(hash ^ code, FNV_PRIME_32) >>> 0;
  }
  chain.push(hash);
  return chain;
}

export function resetLiveCodeSeedsForTest(): void {
  seeds.clear();
  generation = 0;
}

/** Test-only inspection. */
export function __liveCodeSeedStatsForTest(): { entries: number } {
  return { entries: seeds.size };
}
