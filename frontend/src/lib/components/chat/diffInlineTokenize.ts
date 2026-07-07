// Inline diff file-block tokenization dispatcher scoped to a single file
// (one DiffFileBlock = one file = one tokenize batch).
//
// Module-level inFlightKeys dedupes across blocks: if two
// DiffFileBlocks render the same line content (rare but possible —
// boilerplate code, generated headers), only the first dispatch
// queues; the second sees the in-flight claim and skips.
//
import { getSharedDiffHighlighterPool, type DiffTheme } from '../../utils/diffHighlighterPool';
import { getSharedReactiveTokenCache } from '../../utils/tokenCacheReactive.svelte';
import { tokenCacheKeyFromSig, TOKENIZE_MAX_LINE_LENGTH } from '../../utils/tokenCache';
import { stripPatchLinePrefix, type PatchLine } from '../../utils/patchFiles';
import { patchLineSourceKey } from '../../utils/patchLineHash';
import { addToast } from '../../stores/toast.svelte';

const inFlightKeys = new Set<string>();
// Once-per-language guard for the degraded-highlight toast.
// Module-scoped so all inline blocks share it; deliberately not
const warnedTokenizeLanguages = new Set<string>();

/**
 * Queue uncached add/del/context lines for tokenization. Returns when
 * the worker resolves (or fails, in which case the lines stay
 * untokenized — the renderer falls back to plain text via
 * `lineTintClass` backgrounds, so the visual cost is just "no
 * syntax color").
 *
 * Caller should fire-and-forget at mount (`void
 * dispatchInlineFileTokens(...)`); the cache write triggers a
 * generation bump that re-evaluates each line's `getTokens()` lookup.
 */
export async function dispatchInlineFileTokens(
  lines: PatchLine[],
  threadId: string,
  lang: string,
  theme: DiffTheme,
): Promise<void> {
  if (lang === 'plaintext') return;

  const cache = getSharedReactiveTokenCache();
  const seen = new Map<string, string>();
  const claimed: string[] = [];

  for (const line of lines) {
    if (line.type === 'meta' || line.type === 'marker') continue;
    const text = stripPatchLinePrefix(line);
    if (text.length === 0 || text.length > TOKENIZE_MAX_LINE_LENGTH) continue;
    const sourceKey = patchLineSourceKey(line);
    const cacheKey = tokenCacheKeyFromSig(threadId, theme, lang, sourceKey);
    if (cache.get(cacheKey) !== undefined) continue;
    if (inFlightKeys.has(cacheKey)) continue;
    if (seen.has(sourceKey)) continue;
    seen.set(sourceKey, text);
    inFlightKeys.add(cacheKey);
    claimed.push(cacheKey);
  }
  if (seen.size === 0) return;

  try {
    const pool = getSharedDiffHighlighterPool();
    const sourceKeys = Array.from(seen.keys());
    const lineTexts = Array.from(seen.values());
    const tokens = await pool.tokenize({ lines: lineTexts, lang, theme });
    for (let i = 0; i < sourceKeys.length; i += 1) {
      const sk = sourceKeys[i];
      const lineTokens = tokens[i];
      if (sk !== undefined && lineTokens !== undefined) {
        cache.set(tokenCacheKeyFromSig(threadId, theme, lang, sk), lineTokens);
      }
    }
  } catch (err) {
    // Tokenization failures degrade to plain text — already what the
    // renderer does when `getTokens(line) === null`. Logged for
    // diagnostics, plus a one-shot toast per language so the user
    // sees a signal rather than silently-uncolored diff lines.
    reportInlineTokenizeFailure(lang, err);
  } finally {
    for (const key of claimed) inFlightKeys.delete(key);
  }
}

function reportInlineTokenizeFailure(lang: string, err: unknown): void {
  console.warn(`Inline diff tokenize failed for lang=${lang}:`, err);
  if (warnedTokenizeLanguages.has(lang)) return;
  warnedTokenizeLanguages.add(lang);
  addToast('warning', `Syntax highlighting unavailable for ${lang}`);
}
