<script lang="ts" module>
  // Code-block host. Renders the pre/code DOM itself from backend
  // syntax spans (internal/highlight over the HighlightCode RPC) —
  // svelte-streamdown's built-in shiki Code component is out of the
  // pipeline entirely. The wrapper:
  //   1. Stamps the raw source on `data-code-source` so the markdown-
  //      aware copy serializer (`utils/markdownSerialize.ts`) can
  //      recover the original text.
  //   2. Hosts a hover-revealed CopyButton overlay in the top-right
  //      corner — the only visible chrome.
  //
  // Copy contract: the `<code>` textContent equals `token.text`
  // exactly. spanSegments is an exact partition per line and a real
  // `\n` text node joins lines (no display:block line wrappers), so
  // native selection copy matches the source byte-for-byte.
  //
  // Streaming: the volatile tail re-renders this component with
  // growing `token.text`. Requests are serialized on the in-flight
  // one: an idle block fires immediately (after a short floor since
  // the last fire), and content that grows mid-flight coalesces into
  // one drain fire when the flight settles — the color trail behind
  // streamed text is max(floor, round trip + parse), not a fixed
  // throttle window. While a request is pending, the previous
  // response still paints the unchanged prefix lines (minus the
  // last, likely partial, line) and new lines render plain. Each
  // pending window holds a `registerAsyncResource` gate so
  // Streamdown's `onsettled` (the chat warm-gate signal) fires only
  // after spans are cached.
  //
  // Remote clients additionally receive backend-pushed span seeds
  // (`highlight:seed` → liveCodeSeeds.svelte.ts) for streaming fences:
  // the seed's hash chain proves which line prefix of the current text
  // its spans describe, so a matching seed paints ahead of — or
  // entirely instead of — the RPC round trip. Seeds are cache-warmers,
  // never authority: no match, no effect.
  //
  // The gate does NOT delay ChatMarkdown's committed-prefix migration
  // — the boundary splitter commits blocks on markdown structure
  // alone — so a block can remount as committed while its final spans
  // are still in flight. `lastAdopted` bridges that gap: the fresh
  // instance seeds from the previous instance's spans when its text
  // extends them, painting the same stale-prefix colors instead of
  // flashing plain for the round trip.

  import type { EncodedLine } from '../../../utils/syntaxSpans';

  /** Minimum spacing between request fires. Requests serialize on the
   * in-flight one, so this floor only matters when the round trip is
   * faster than it — it keeps a local backend from being asked to
   * re-parse the same growing block on every streamed chunk. */
  const MIN_FIRE_INTERVAL_MS = 25;

  // marked exposes token.lang as the FULL trimmed info string
  // ("py title=x"); highlight identity is its first word — matching
  // the backend fence scanner's seed keys (highlight.infoLang) and
  // giving the backend's LangFromName a resolvable name. The full
  // string stays on data-code-lang for fence-faithful serialization.
  export function infoWord(lang: string): string {
    for (let i = 0; i < lang.length; i += 1) {
      const ch = lang[i];
      if (ch === ' ' || ch === '\t') return lang.slice(0, i);
    }
    return lang;
  }

  // Last adopted spans per language, for remount seeding (see above).
  // Fence languages are arbitrary first words of info strings, so the
  // map is LRU-capped, and entries retain the FULL block source — a
  // count cap alone could hold megabytes of dead text from destroyed
  // blocks, so a total char budget backs it up. Empty span sets
  // (truncated over-cap results) are not worth remembering: reseeding
  // them paints nothing.
  const LAST_ADOPTED_MAX = 8;
  const LAST_ADOPTED_MAX_TOTAL_CHARS = 512 * 1024;
  const lastAdopted = new Map<string, { text: string; spans: EncodedLine[] }>();
  let lastAdoptedChars = 0;

  function rememberAdoption(lang: string, text: string, spans: EncodedLine[]): void {
    if (spans.length === 0) return;
    const prior = lastAdopted.get(lang);
    if (prior) {
      lastAdoptedChars -= prior.text.length;
      lastAdopted.delete(lang);
    }
    lastAdopted.set(lang, { text, spans });
    lastAdoptedChars += text.length;
    while (lastAdopted.size > LAST_ADOPTED_MAX || lastAdoptedChars > LAST_ADOPTED_MAX_TOTAL_CHARS) {
      const oldest = lastAdopted.keys().next().value;
      if (oldest === undefined) break;
      lastAdoptedChars -= lastAdopted.get(oldest)?.text.length ?? 0;
      lastAdopted.delete(oldest);
    }
  }

  export function __resetStreamdownCodeHostForTest(): void {
    lastAdopted.clear();
    lastAdoptedChars = 0;
  }

  export function __streamdownCodeHostStatsForTest(): { lastAdopted: number; chars: number } {
    return { lastAdopted: lastAdopted.size, chars: lastAdoptedChars };
  }
</script>

<script lang="ts">
  import { useStreamdown } from 'svelte-streamdown';
  import type { Tokens } from 'marked';
  import { onDestroy, untrack } from 'svelte';
  import CopyButton from '../../primitives/CopyButton.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { spanSegments } from '../../../utils/syntaxSpans';
  import { getCachedBlockSpans, requestBlockSpans } from './codeSpanCache';
  import { liveCodeSeedGeneration, matchLiveCodeSeed } from './liveCodeSeeds.svelte';

  let { token, id }: { token: Tokens.Code; id: string } = $props();

  const streamdown = useStreamdown();

  // Highlight identity for this block (see infoWord above). Everything
  // span-related — cache keys, seed matching, RPC lang — uses this;
  // only the data-code-lang stamp keeps the full info string.
  let highlightLang = $derived(infoWord(token.lang ?? ''));

  // Resolved spans plus the exact (lang, source) they were computed
  // for. Language is part of the identity: the same text under a new
  // fence language must re-request, not keep the old classes.
  let spans = $state<EncodedLine[] | null>(null);
  let spansFor = $state('');
  let spansForLang = $state('');

  let timer: ReturnType<typeof setTimeout> | null = null;
  let inFlight = false;
  let lastFireAt = 0;
  let pendingLang = '';
  let pendingText = '';
  // True while the pending (lang, text) still needs a fire. Set by
  // schedule(), consumed when a fire takes the pending content, and
  // cleared by cancelScheduled() — a synchronous adoption satisfies
  // the current content, so nothing is owed.
  let pendingDirty = false;
  // Bumped for every fire and every synchronous adoption; a resolving
  // request only adopts if it is still the newest. Supersession by
  // sequence, not source length — a block whose text is REPLACED with
  // shorter content (design previews, non-append rerenders) must
  // still converge.
  let fireSeq = 0;
  let releaseGate: (() => void) | null = null;
  let destroyed = false;

  function holdGate(): void {
    if (destroyed) return;
    releaseGate ??= streamdown.registerAsyncResource?.() ?? null;
  }

  // The gate tracks the CURRENT content's async work only: it releases
  // when no fire is queued or in flight. Superseded fires
  // (seq !== fireSeq) neither adopt nor settle the gate — a stalled
  // stale request must not block the parent's settle signal when the
  // content it was for is long gone; cancelScheduled() releases
  // directly for that case after bumping the seq.
  function maybeReleaseGate(): void {
    if (timer === null && !inFlight && releaseGate) {
      releaseGate();
      releaseGate = null;
    }
  }

  function adopt(lang: string, text: string, result: EncodedLine[] | null): void {
    spans = result;
    spansFor = text;
    spansForLang = lang;
    if (result) rememberAdoption(lang, text, result);
  }

  // A synchronous adoption (cache hit, language-less fence, already
  // exact) satisfies the current content: any queued fire is stale
  // (its timer is cleared), any in-flight fire is demoted by the seq
  // bump, and nothing further is owed (pendingDirty clears — an
  // in-flight fire's drain check must not resurrect the obsolete
  // pending content). The gate releases directly, not through
  // maybeReleaseGate: a demoted in-flight request may still be
  // pending, and it must not keep the gate held.
  function cancelScheduled(): void {
    fireSeq += 1;
    pendingDirty = false;
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    if (releaseGate) {
      releaseGate();
      releaseGate = null;
    }
  }

  function schedule(lang: string, text: string): void {
    pendingLang = lang;
    pendingText = text;
    pendingDirty = true;
    holdGate();
    // Serialize on the in-flight request: its finally drains the
    // pending content. A queued timer already covers it.
    if (inFlight || timer !== null) return;
    queueFire();
  }

  function queueFire(): void {
    const sinceLast = performance.now() - lastFireAt;
    if (sinceLast >= MIN_FIRE_INTERVAL_MS) {
      void fire();
      return;
    }
    timer = setTimeout(() => {
      timer = null;
      void fire();
    }, MIN_FIRE_INTERVAL_MS - sinceLast);
  }

  async function fire(): Promise<void> {
    inFlight = true;
    pendingDirty = false;
    lastFireAt = performance.now();
    const seq = ++fireSeq;
    const lang = pendingLang;
    const text = pendingText;
    try {
      const result = await requestBlockSpans(lang, text);
      if (!destroyed && result && seq === fireSeq) {
        adopt(lang, text, result);
      }
    } finally {
      inFlight = false;
      if (!destroyed && pendingDirty) {
        // Content advanced mid-flight (or after a synchronous
        // adoption demoted this fire): drain it. Not seq-gated — the
        // dirty flag is authoritative for whether newer content still
        // needs a request, and skipping the drain here would strand
        // it (and the held gate) until the next token change.
        queueFire();
      } else if (seq === fireSeq) {
        maybeReleaseGate();
      }
    }
  }

  $effect(() => {
    const text = token.text;
    const lang = highlightLang;
    // Tracked alongside the token: a backend-pushed seed (remote
    // clients, highlight:seed) can arrive BETWEEN token changes — e.g.
    // the final seed after the last delta — and must re-run the match
    // below. Loopback clients never receive seeds, so this stays 0.
    liveCodeSeedGeneration();
    untrack(() => {
      if (spansForLang === lang && spansFor === text && spans !== null) {
        // Already exact for the current token: any queued or in-flight
        // fire is for an older token and must not adopt over this.
        cancelScheduled();
        return;
      }
      if (!lang) {
        // No fence language → definitively plain; skip the round trip.
        cancelScheduled();
        adopt(lang, text, null);
        return;
      }
      const hit = getCachedBlockSpans(lang, text);
      if (hit) {
        cancelScheduled();
        adopt(lang, text, hit);
        return;
      }
      // Backend-pushed live seed (remote clients): the hash chain
      // verifies exactly which line prefix of THIS text the seed's
      // spans describe. An exact match settles the block without any
      // RPC; a partial match paints the verified prefix through the
      // existing stale-prefix rendering while the exact request runs.
      const seed = matchLiveCodeSeed(lang, text);
      if (seed?.exact) {
        cancelScheduled();
        adopt(lang, text, seed.spans);
        return;
      }
      if (spans === null) {
        // Fresh instance (committed-prefix remount, settle rerender):
        // seed from the previous instance's adoption when this text
        // extends it, so the block keeps its colors while the exact
        // spans are fetched.
        const last = lastAdopted.get(lang);
        if (last && text.startsWith(last.text)) {
          spans = last.spans;
          spansFor = last.text;
          spansForLang = lang;
        }
      }
      if (seed) {
        // Adopt the seed's verified prefix only when it covers MORE of
        // the current text than whatever spans this instance already
        // paints from (its own last response or the lastAdopted seed).
        const currentCoverage =
          spansForLang === lang && spans !== null && text.startsWith(spansFor)
            ? spansFor.length
            : 0;
        if (seed.covered.length > currentCoverage) {
          // Adopt WITH the trailing newline: the hash walk verified the
          // covered lines as complete up to a '\n' in this text, and
          // keeping that newline in spansFor both preserves the
          // alignment guarantee under later replacements (startsWith
          // fails if the newline is gone) and tells lineSpans() the
          // final span line is complete, not mid-growth.
          adopt(lang, seed.covered + '\n', seed.spans);
        }
      }
      schedule(lang, text);
    });
  });

  onDestroy(() => {
    destroyed = true;
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    // Release immediately: this block's spans no longer gate anything,
    // and a stalled request must not hold the parent's settle signal.
    // fire()'s finally sees releaseGate === null and no-ops.
    if (releaseGate) {
      releaseGate();
      releaseGate = null;
    }
  });

  let lines = $derived(token.text.split('\n'));
  let exact = $derived(spansForLang === highlightLang && spansFor === token.text);
  // Stale spans are only trustworthy when the current text EXTENDS the
  // source they were computed for (streaming append). A replacement
  // (different language, rewritten block) must render plain until its
  // own result lands — especially since that result can reject.
  let staleUsable = $derived(spansForLang === highlightLang && token.text.startsWith(spansFor));

  function lineSpans(index: number): EncodedLine | null {
    if (!spans) return null;
    if (exact) return spans[index] ?? null;
    if (!staleUsable) return null;
    // Stale spans mid-stream: trust the prefix, but drop the last span
    // line when the source it covered may have still been growing. A
    // spansFor ending in '\n' (seed adoption) proves every content
    // line it covers is complete, so nothing needs dropping.
    const completeLines = spansFor.endsWith('\n') ? spans.length : spans.length - 1;
    return index < completeLines ? (spans[index] ?? null) : null;
  }
</script>

<!-- Named Tailwind group (`group/codeblock`) so the hover scope is
     this code block specifically. Unnamed `group-hover:` would also
     match an outer `class="group"` (AssistantMessage.svelte has one
     for its timestamp/copy-row chrome), revealing every code block's
     copy button whenever any part of the message is hovered. -->
<div
  class="streamdown-code-host group/codeblock relative"
  data-code-source={token.text}
  data-code-lang={token.lang ?? ''}
>
  <div
    data-streamdown-code={id}
    style={streamdown.isMounted ? streamdown.animationBlockStyle : ''}
    class={streamdown.theme.code.base}
  >
    <div style="height: fit-content; width: 100%;" class={streamdown.theme.code.container}>
      <pre class={streamdown.theme.code.pre}><code
          >{#each lines as lineText, lineIndex (lineIndex)}{#if lineIndex > 0}{'\n'}{/if}{#each spanSegments(lineText, lineSpans(lineIndex)) as seg, segIndex (segIndex)}{#if seg.className}<span class={seg.className}>{seg.text}</span>{:else}{seg.text}{/if}{/each}{/each}</code
        ></pre>
    </div>
  </div>

  <div
    class="absolute top-1 right-1 z-10 opacity-0 transition-opacity duration-150 ease-out group-hover/codeblock:opacity-100 focus-within:opacity-100"
  >
    <CopyButton
      text={token.text}
      label="Copy code"
      onError={() => addToast('error', 'Failed to copy')}
    />
  </div>
</div>
