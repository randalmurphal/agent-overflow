// Module-level span cache for markdown code blocks (same precedent as
// the KaTeX HTML / Mermaid SVG caches in the vendor patch): the
// committed-prefix / volatile-tail split in ChatMarkdown remounts each
// settled block once, and the synchronous `getCachedBlockSpans` hit
// makes that migration flash-free — the remounted instance paints
// highlighted on its first render instead of flashing plain.
//
// Keys are content-addressed `(lang, fnv1a(source))`; spans are
// theme-independent, so a theme toggle costs nothing. Success is
// cached — including all-plain results for unknown languages, which
// are the backend's authoritative answer — while rejections and
// incomplete results (transient parse degradation) are never cached,
// so a transient failure retries on the next request.

import { HighlightCode } from '../../../stores/bindings';
import { addToast } from '../../../stores/toast.svelte';
import { appendFNV1a32, contentKey, fnv1a32 } from '../../../utils/fnv1a';
import { ensureSyntaxClassNames, type EncodedLine } from '../../../utils/syntaxSpans';
import { matchesProvenAppend, type ProvenAppend } from 'svelte-streamdown';

/** Entry cap. Blocks are small (plain lines carry no runs); 300
 * comfortably covers every visible message's blocks plus the settle
 * remount window. LRU by Map insertion order. */
export const CODE_SPAN_CACHE_MAX_ENTRIES = 300;

const cache = new Map<string, EncodedLine[]>();
const inFlight = new Map<string, Promise<EncodedLine[] | null>>();

// Once-per-language guard for the degraded-highlight toast.
const warnedLanguages = new Set<string>();

/**
 * Immutable source plus its already-computed content key. Streaming code hosts
 * extend this identity from an opaque append proof, so cache lookups and
 * throttled requests hash only the new suffix instead of the whole open fence.
 */
export interface CodeSourceIdentity {
  readonly source: string;
  readonly contentKey: string;
  readonly hash: number;
}

const codeSourceIdentities = new WeakSet<CodeSourceIdentity>();

function mintCodeSourceIdentity(source: string, hash: number): CodeSourceIdentity {
  const identity = Object.freeze({
    source,
    contentKey: `${source.length}:${hash.toString(36)}`,
    hash,
  });
  codeSourceIdentities.add(identity);
  return identity;
}

export function createCodeSourceIdentity(source: string): CodeSourceIdentity {
  return mintCodeSourceIdentity(source, fnv1a32(source));
}

export function appendCodeSourceIdentity(
  identity: CodeSourceIdentity,
  append: ProvenAppend,
): CodeSourceIdentity {
  if (
    !codeSourceIdentities.has(identity) ||
    !matchesProvenAppend(append, identity.source, append.next)
  ) {
    throw new Error('Code source identity append does not match its current source');
  }
  return mintCodeSourceIdentity(append.next, appendFNV1a32(identity.hash, append.delta));
}

function requireCodeSourceIdentity(identity: CodeSourceIdentity): CodeSourceIdentity {
  if (!codeSourceIdentities.has(identity)) {
    throw new Error('Code span cache requires an identity minted by codeSpanCache');
  }
  return identity;
}

// NUL separator: fence languages are arbitrary info-string text, so a
// visible separator could collide (`a b` + key vs `a` + `b <key>`).
function keyFor(lang: string, sourceContentKey: string): string {
  return `${lang}\0${sourceContentKey}`;
}

function keyOf(lang: string, source: string): string {
  return keyFor(lang, contentKey(source));
}

function touch(key: string, spans: EncodedLine[]): void {
  cache.delete(key);
  cache.set(key, spans);
}

function insert(key: string, spans: EncodedLine[]): void {
  touch(key, spans);
  while (cache.size > CODE_SPAN_CACHE_MAX_ENTRIES) {
    const oldest = cache.keys().next().value;
    if (oldest === undefined) break;
    cache.delete(oldest);
  }
}

/**
 * Ingests a backend-pushed FINAL seed (`highlight:seed`, remote
 * clients only): the spans for a fence whose content is final, keyed
 * by the `contentKey(source)` the backend computed with frontend hash
 * parity. A later mount of the exact content is a synchronous hit —
 * no RPC. The caller (eventsHighlight) has already awaited the class-
 * name table and filtered incomplete results.
 */
export function seedFinalBlockSpans(
  lang: string,
  sourceContentKey: string,
  spans: EncodedLine[],
): void {
  if (!lang || !sourceContentKey) return;
  insert(keyFor(lang, sourceContentKey), spans);
}

/**
 * Synchronous cache read. The settle-remount path depends on this:
 * a block whose exact content was highlighted before renders correct
 * spans on its first paint, no async gap.
 */
export function getCachedBlockSpans(lang: string, source: string): EncodedLine[] | null {
  const key = keyOf(lang, source);
  return getCachedByKey(key);
}

export function getCachedBlockSpansByIdentity(
  lang: string,
  identity: CodeSourceIdentity,
): EncodedLine[] | null {
  const source = requireCodeSourceIdentity(identity);
  return getCachedByKey(keyFor(lang, source.contentKey));
}

function getCachedByKey(key: string): EncodedLine[] | null {
  const hit = cache.get(key);
  if (!hit) return null;
  touch(key, hit);
  return hit;
}

/**
 * Fetches spans for one code block, deduping concurrent requests for
 * identical content (the tail instance's final request and the
 * committed instance's remount request are usually the same content).
 * Resolves null on failure — logged plus a one-shot toast per
 * language — and never caches it.
 */
export function requestBlockSpans(lang: string, source: string): Promise<EncodedLine[] | null> {
  return requestBlockSpansByKey(lang, source, keyOf(lang, source));
}

export function requestBlockSpansByIdentity(
  lang: string,
  identity: CodeSourceIdentity,
): Promise<EncodedLine[] | null> {
  const source = requireCodeSourceIdentity(identity);
  return requestBlockSpansByKey(lang, source.source, keyFor(lang, source.contentKey));
}

function requestBlockSpansByKey(
  lang: string,
  source: string,
  key: string,
): Promise<EncodedLine[] | null> {
  const hit = cache.get(key);
  if (hit) {
    touch(key, hit);
    return Promise.resolve(hit);
  }
  const pending = inFlight.get(key);
  if (pending) return pending;

  const request = (async (): Promise<EncodedLine[] | null> => {
    try {
      const result = await HighlightCode({ lang, source });
      // Never resolve spans against an empty class-name table; the
      // id → class map loads once per page load.
      await ensureSyntaxClassNames();
      const spans = result.lines ?? [];
      // Incomplete results (parse timeout under load) are transient:
      // the backend declines to memoize them so a retry can succeed,
      // and so does this cache. The host still adopts the returned
      // spans into its local state, so display is unaffected; the next
      // mount of this content re-requests instead of pinning the
      // partial result for the page lifetime.
      if (!result.incomplete) {
        insert(key, spans);
      }
      return spans;
    } catch (err) {
      console.warn(`Code-block highlight failed for lang=${lang}:`, err);
      if (!warnedLanguages.has(lang)) {
        warnedLanguages.add(lang);
        addToast('warning', `Syntax highlighting unavailable for ${lang}`);
      }
      return null;
    } finally {
      inFlight.delete(key);
    }
  })();
  inFlight.set(key, request);
  return request;
}

export function resetCodeSpanCacheForTest(): void {
  cache.clear();
  inFlight.clear();
  warnedLanguages.clear();
}

/** Test-only inspection. */
export function __codeSpanCacheStatsForTest(): { entries: number } {
  return { entries: cache.size };
}

/** Diagnostic accounting (memoryReport). `approxKeyChars` is only the compact
 * content-address key storage. Source and encoded-span payloads are not
 * retained here in a form this synchronous report can size cheaply. */
export function codeSpanCacheStats(): { entries: number; approxKeyChars: number } {
  let approxKeyChars = 0;
  for (const key of cache.keys()) approxKeyChars += key.length;
  return { entries: cache.size, approxKeyChars };
}
