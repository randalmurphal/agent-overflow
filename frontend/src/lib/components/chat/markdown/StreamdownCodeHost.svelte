<script lang="ts" module>
  // Code-block host. Renders the pre/code DOM itself from backend
  // syntax spans (internal/highlight over the HighlightCode RPC). This
  // is the ONLY code renderer: the renderer's shiki-backed Code
  // component was deleted with the rest of its dead chrome. The wrapper:
  //   1. Keeps a source-free `data-code-source` marker for code-block
  //      discovery. The DOM text and CopyButton already own the source, so
  //      duplicating a growing block into an attribute only wastes Oilpan
  //      memory.
  //   2. Hosts a hover-revealed CopyButton overlay in the top-right
  //      corner — the only visible chrome.
  //
  // Copy contract: the `<code>` textContent equals `token.text`
  // exactly. spanSegments is an exact partition per line and a real
  // `\n` text node joins lines (no display:block line wrappers), so
  // native selection copy matches the source byte-for-byte.
  //
  // Streaming: the volatile tail re-renders this component with
  // growing `token.text`. A block of a streaming assistant row paints
  // from the spans the backend pushes for the row's fences
  // (`highlight:live` → liveCodeSpans.svelte.ts): the fence's line-hash
  // chain proves which lines of this text its spans describe, the
  // partial last line takes its line's spans clipped, and no request
  // runs while a fence covers the text. A fence's first push arrives
  // before its text, so a followed block never requests spans; a fence
  // takes over from requested colors only once it colors as many lines.
  // A block no fence covers requests spans instead. Requests are
  // serialized on the in-flight one: an idle block fires immediately
  // (after a short floor since the last fire), and content that grows
  // mid-flight coalesces into one drain fire when the flight settles.
  // While a request is
  // pending, the previous spans still paint the prefix they describe,
  // their last line clipped to what it was, and new text renders
  // plain; colors never leave text they were proven for. Each pending
  // window holds a `registerAsyncResource` gate so Streamdown's
  // `onsettled` (the chat warm-gate signal) fires only after spans are
  // cached.
  // A completed host never replaces itself with static HTML. CompactBlocks is
  // the sole retirement owner, and span adoption waits while a native text
  // selection intersects this block so highlighting cannot erase the range.
  //
  // The gate does NOT delay ChatMarkdown's committed-prefix migration
  // — the boundary splitter commits blocks on markdown structure
  // alone — so a block can remount as committed while its final spans
  // are still in flight. `lastAdopted` bridges that gap: the fresh
  // instance seeds from the previous instance's spans when its text
  // extends them, painting the same stale-prefix colors instead of
  // flashing plain for the round trip.

  import type { EncodedLine } from '../../../utils/syntaxSpans';
  import { resetCompletedCodeBlockRenderersForTest } from './staticCodeBlock';

  /** Minimum spacing between request fires. Requests serialize on the
   * in-flight one, so this floor only matters when the round trip is
   * faster than it — it keeps a local backend from being asked to
   * re-parse the same growing block on every streamed chunk. */
  const MIN_FIRE_INTERVAL_MS = 25;

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
    resetCompletedCodeBlockRenderersForTest();
  }

  export function __streamdownCodeHostStatsForTest(): { lastAdopted: number; chars: number } {
    return { lastAdopted: lastAdopted.size, chars: lastAdoptedChars };
  }
</script>

<script lang="ts">
  import {
    acquireDocumentInteraction,
    matchesProvenAppend,
    useStreamdown,
    type DocumentInteraction,
    type ProvenAppend,
  } from '../../../markdown';
  import type { Tokens } from '../../../markdown';
  import { onDestroy, onMount, untrack } from 'svelte';
  import TextWrap from '@lucide/svelte/icons/text-wrap';
  import CopyButton from '../../primitives/CopyButton.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import IconButton from '../../primitives/IconButton.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import {
    appendCodeSourceIdentity,
    createCodeSourceIdentity,
    getCachedBlockSpansByIdentity,
    requestBlockSpansByIdentity,
    type CodeSourceIdentity,
  } from './codeSpanCache';
  import {
    clearCompletedCodeBlockRenderer,
    codeFenceInfoWord,
    codeWrapLabel,
    publishCompletedCodeBlockRenderer,
    renderStaticCodeBlockHtml,
  } from './staticCodeBlock';
  import { isCodeBlockUnwrappedByKey, setCodeBlockUnwrappedByKey } from './codeWrapState';
  import {
    coverColorsWhole,
    coverLiveCode,
    coverPaints,
    getLiveCodeRowContext,
    liveCodeRow,
    liveCoverSpans,
    TextLineChain,
    type LiveCodeCover,
  } from './liveCodeSpans.svelte';
  import { CodeLines } from './codeLines.svelte';

  let {
    token,
    id,
    textAppend,
  }: {
    token: Tokens.Code;
    id: string;
    textAppend?: ProvenAppend;
  } = $props();

  const streamdown = useStreamdown();

  // Highlight identity for this block (see infoWord above). Everything
  // span-related — cache keys, pushed-span matching, RPC lang — uses this;
  // only the data-code-lang stamp keeps the full info string.
  let highlightLang = $derived(codeFenceInfoWord(token.lang ?? ''));

  // Resolved spans plus the exact (lang, source) they were computed
  // for. Language is part of the identity: the same text under a new
  // fence language must re-request, not keep the old classes.
  let spans = $state.raw<EncodedLine[] | null>(null);
  let spansFor = $state('');
  let spansForLang = $state('');
  let staleUsable = $state(false);

  // CodeLines renders the lines and re-renders only the lines a step changes.
  const initialText = untrack(() => token.text);
  let renderedText = initialText;
  let renderedLang = untrack(() => highlightLang);
  let sourceIdentity = createCodeSourceIdentity(initialText);
  const codeLines = new CodeLines(initialText);
  // The row this block streams in, and the line-hash chain of its text that
  // proves which pushed spans describe it.
  const liveRowRef = getLiveCodeRowContext();
  const lineChain = new TextLineChain(initialText);
  // The pushed fence painting this block, while one covers its text.
  let live = $state.raw<LiveCodeCover | null>(null);
  let codeRoot = $state<HTMLElement>();
  // Per-block wrap choice. Seeded from the keyed record so a remounted or
  // re-rendered block keeps the reader's choice; the toggle records the
  // CURRENT identity, and the completed renderer reads this state when the
  // island retires, so a choice made on a still-streaming block also lands
  // in its static HTML.
  let unwrapped = $state(isCodeBlockUnwrappedByKey(renderedLang, sourceIdentity.contentKey));

  function toggleWrap(): void {
    unwrapped = !unwrapped;
    setCodeBlockUnwrappedByKey(highlightLang, sourceIdentity.contentKey, unwrapped);
  }
  const completedRendererOwner = {};
  // While the block streams, each of its span results replaces the previous
  // one in the span cache (see codeSpanCache.ts).
  const streamingCacheOwner = {};
  function spanCacheOwner(): object | undefined {
    return streamdown.parseIncompleteMarkdown === false ? undefined : streamingCacheOwner;
  }
  let documentInteraction: DocumentInteraction | undefined;
  let pendingAdoption: {
    lang: string;
    text: string;
    result: EncodedLine[] | null;
  } | undefined;
  let codeDiagnostics: {
    readonly tokenText: string;
    readonly renderedText: string;
    readonly renderedLines: string;
    readonly spansFor: string;
    readonly spansForLang: string;
  } | undefined;
  $effect.pre(() => {
    const text = token.text;
    const lang = highlightLang;
    if (text !== renderedText) {
      const appended = matchesProvenAppend(textAppend, renderedText, text);
      if (appended) {
        untrack(() => codeLines.append(textAppend.delta));
        lineChain.append(textAppend.delta);
        sourceIdentity = appendCodeSourceIdentity(sourceIdentity, textAppend);
        staleUsable = spans !== null &&
          spansForLang === lang &&
          (staleUsable || spansFor === renderedText);
      } else {
        untrack(() => codeLines.replace(text));
        lineChain.reset(text);
        sourceIdentity = createCodeSourceIdentity(text);
        staleUsable = spans !== null &&
          spansForLang === lang &&
          text.startsWith(spansFor);
      }
      renderedText = text;
    }
    if (lang !== renderedLang) {
      staleUsable = spans !== null &&
        spansForLang === lang &&
        text.startsWith(spansFor);
      renderedLang = lang;
    }
  });

  let timer: ReturnType<typeof setTimeout> | null = null;
  let inFlight = false;
  let lastFireAt = 0;
  let pendingLang = '';
  let pendingSource = sourceIdentity;
  // True while the pending (lang, text) still needs a fire. Set by
  // schedule(), consumed when a fire takes the pending content, and
  // cleared by cancelScheduled() — a synchronous adoption satisfies
  // the current content, so nothing is owed.
  let pendingDirty = false;
  // Bumped for every fire and every synchronous adoption; a resolving
  // request only adopts if it is still the newest. Supersession by
  // sequence, not source length — a block whose text is REPLACED with
  // shorter content (such as non-append rerenders) must
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

  function selectionOwnsCode(): boolean {
    if (!documentInteraction || !codeRoot) return false;
    if (documentInteraction.selectionPending) return true;
    for (const interactionRange of documentInteraction.ranges) {
      if (
        interactionRange.endpointAncestors.has(codeRoot) ||
        interactionRange.range.intersectsNode(codeRoot)
      ) return true;
    }
    return false;
  }

  function focusOwnsCode(): boolean {
    if (!documentInteraction || !codeRoot) return false;
    if (documentInteraction.focusedAncestors.has(codeRoot)) return true;
    const active = codeRoot.ownerDocument.activeElement;
    return active !== null && codeRoot.contains(active);
  }

  function applyAdoption(lang: string, text: string, result: EncodedLine[] | null): void {
    spans = result;
    spansFor = text;
    spansForLang = lang;
    const currentText = token.text;
    staleUsable = result !== null &&
      lang === highlightLang &&
      (text === currentText || currentText.startsWith(text));
    if (result) rememberAdoption(lang, text, result);
    publishCompletedRenderer(lang, text);
  }

  function adopt(lang: string, text: string, result: EncodedLine[] | null): void {
    if (selectionOwnsCode()) {
      pendingAdoption = { lang, text, result };
      return;
    }
    pendingAdoption = undefined;
    if (
      streamdown.parseIncompleteMarkdown === false &&
      lang === highlightLang &&
      text === token.text &&
      !focusOwnsCode()
    ) {
      if (result) rememberAdoption(lang, text, result);
      // The parent can retire this completed island immediately. Publishing
      // the exact renderer avoids first building the same syntax-span DOM in
      // Svelte and then replacing it with identical static DOM one microtask
      // later.
      publishCompletedRenderer(lang, text, result);
      return;
    }
    applyAdoption(lang, text, result);
  }

  function flushPendingAdoption(): void {
    const pending = pendingAdoption;
    if (!pending || selectionOwnsCode()) return;
    pendingAdoption = undefined;
    adopt(pending.lang, pending.text, pending.result);
  }

  function publishCompletedRenderer(
    lang: string,
    text: string,
    settledSpans?: readonly EncodedLine[] | null,
  ): void {
    if (
      streamdown.parseIncompleteMarkdown !== false ||
      lang !== highlightLang ||
      text !== token.text
    ) return;
    const staticLineSpans = settledSpans === undefined
      ? lineSpans
      : (index: number): EncodedLine | null => settledSpans?.[index] ?? null;
    // A choice made while the block was still streaming was recorded under
    // an earlier identity; record it under the final one so a remount from
    // the span cache (no host) renders it the same way.
    if (unwrapped) setCodeBlockUnwrappedByKey(lang, sourceIdentity.contentKey, true);
    publishCompletedCodeBlockRenderer(
      completedRendererOwner,
      streamdown,
      lang,
      text,
      (staticID) =>
        renderStaticCodeBlockHtml(token, staticID, streamdown, codeLines.texts(), staticLineSpans, unwrapped),
    );
    streamdown.requestStaticRetry();
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

  function schedule(lang: string, source: CodeSourceIdentity): void {
    if (streamdown.parseIncompleteMarkdown === false) {
      clearCompletedCodeBlockRenderer(completedRendererOwner);
    }
    pendingLang = lang;
    pendingSource = source;
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
    const source = pendingSource;
    const text = source.source;
    try {
      const result = await requestBlockSpansByIdentity(lang, source, spanCacheOwner());
      if (!destroyed && result && seq === fireSeq) {
        adopt(lang, text, result);
      } else if (!destroyed && result === null && seq === fireSeq) {
        // Preserve the component's current plain/stale rendering for the
        // parent-owned compaction pass. A failure stays uncached, so a future
        // mount still retries the backend.
        publishCompletedRenderer(lang, text);
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
    const source = sourceIdentity;
    // Tracked alongside the token: a push for the row can arrive BETWEEN
    // token changes (the final push after the last delta) and must re-run
    // the cover below.
    const ref = liveRowRef?.();
    const row = ref ? liveCodeRow(ref.threadId, ref.itemId) : undefined;
    if (streamdown.diagnostics && codeRoot && !codeDiagnostics) {
      const root = codeRoot as HTMLElement & { __aoCodeDiagnostics?: unknown };
      codeDiagnostics = {
        get tokenText() { return token.text; },
        get renderedText() { return renderedText; },
        get renderedLines() { return codeLines.texts().join('\n'); },
        get spansFor() { return spansFor; },
        get spansForLang() { return spansForLang; },
      };
      Object.defineProperty(root, '__aoCodeDiagnostics', {
        configurable: true,
        value: codeDiagnostics,
      });
    }
    untrack(() => {
      if (spansForLang === lang && spansFor === text && spans !== null) {
        // Already exact for the current token: any queued or in-flight
        // fire is for an older token and must not adopt over this.
        live = null;
        cancelScheduled();
        return;
      }
      if (!lang) {
        // No fence language → definitively plain; skip the round trip.
        live = null;
        cancelScheduled();
        adopt(lang, text, null);
        return;
      }
      const hit = getCachedBlockSpansByIdentity(lang, source, spanCacheOwner());
      if (hit) {
        live = null;
        cancelScheduled();
        adopt(lang, text, hit);
        return;
      }
      const streaming = streamdown.parseIncompleteMarkdown !== false;
      const cover = row || live ? coverLiveCode(row, live?.fence, lang, lineChain, streaming) : null;
      if (cover?.exact) {
        live = null;
        cancelScheduled();
        adopt(lang, text, cover.fence.lines);
        return;
      }
      // Another fence's cover, or one replacing requested colors, takes over
      // only once it colors in full every line the block's colors reach;
      // until then the block keeps requesting its own.
      if (cover && ((live && cover.fence === live.fence) || coverColorsWhole(cover) >= paintedLines())) {
        live = cover;
        cancelScheduled();
        return;
      }
      if (live) {
        // The pushed fence no longer describes this text (it stopped, or
        // the text left it): its verified lines keep their colors while
        // the request runs.
        keepLiveColors(live, lang);
        live = null;
      }
      if (text === '') {
        // Nothing to color yet, and a fence's first push precedes its text.
        cancelScheduled();
        adopt(lang, text, null);
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
          staleUsable = true;
        }
      }
      schedule(lang, source);
    });
  });

  /** Leading lines the block's current colors cover. */
  function paintedLines(): number {
    if (live) return coverPaints(live);
    if (!spans || spansForLang !== highlightLang || !(exact || staleUsable)) return 0;
    return Math.min(spans.length, lineChain.lineCount);
  }

  /** Keeps the lines a cover painted as the stale spans the next request
   * replaces. They are copied: an open fence changes in place. spansFor is
   * kept a strict prefix of the text, so the block still requests its own
   * spans. */
  function keepLiveColors(cover: LiveCodeCover, lang: string): void {
    const text = token.text;
    spans = cover.fence.lines.slice(0, coverPaints(cover));
    spansFor = codeLines.texts().slice(0, cover.verified).join('\n');
    if (spansFor === text) spansFor = text.slice(0, -1);
    spansForLang = lang;
    staleUsable = text.startsWith(spansFor);
  }

  onMount(() => {
    const ownerDocument = codeRoot?.ownerDocument ?? document;
    documentInteraction = acquireDocumentInteraction(ownerDocument, flushPendingAdoption);
    flushPendingAdoption();
    return () => {
      documentInteraction?.release();
      documentInteraction = undefined;
    };
  });

  onDestroy(() => {
    destroyed = true;
    pendingAdoption = undefined;
    clearCompletedCodeBlockRenderer(completedRendererOwner);
    const root = codeRoot as (HTMLElement & { __aoCodeDiagnostics?: unknown }) | undefined;
    if (root && root.__aoCodeDiagnostics === codeDiagnostics) {
      delete root.__aoCodeDiagnostics;
    }
    codeDiagnostics = undefined;
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

  let exact = $derived(spansForLang === highlightLang && spansFor === token.text);
  // Stale spans are only trustworthy when the current text EXTENDS the
  // source they were computed for (streaming append). A replacement
  // (different language, rewritten block) must render plain until its
  // own result lands — especially since that result can reject.

  function lineSpans(index: number): EncodedLine | null {
    if (live) return liveCoverSpans(live, index);
    if (!spans) return null;
    if (exact) return spans[index] ?? null;
    if (!staleUsable) return null;
    // Stale spans describe a prefix of this text. A last line that has
    // grown since keeps its runs over the part they were computed for.
    return spans[index] ?? null;
  }

  $effect.pre(() => codeLines.paint(lineSpans));

</script>

<!-- Named Tailwind group (`group/codeblock`) so the hover scope is
     this code block specifically. Unnamed `group-hover:` would also
     match an outer `class="group"` (AssistantMessage.svelte has one
     for its timestamp/copy-row chrome), revealing every code block's
     copy button whenever any part of the message is hovered. -->
<div
  bind:this={codeRoot}
  class="streamdown-code-host group/codeblock relative"
  data-code-source=""
  data-code-lang={token.lang ?? ''}
  data-code-unwrapped={unwrapped ? '' : undefined}
>
  <div
    data-streamdown-code={id}
    class={streamdown.theme.code.base}
  >
    <div style="height: fit-content; width: 100%;" class={streamdown.theme.code.container}>
      <pre class={streamdown.theme.code.pre}><code
          >{#each codeLines.lines as line, lineIndex (lineIndex)}{#if lineIndex > 0}{'\n'}{/if}{#each line.segments as seg, segIndex (segIndex)}{#if seg.className}<span class={seg.className}>{seg.text}</span>{:else}{seg.text}{/if}{/each}{/each}</code
        ></pre>
    </div>
  </div>

  <!-- Keep in step with CODE_OVERLAY_CLASS in staticCodeBlock.ts: the settled
       block renders the same overlay as static HTML. -->
  <div
    class="absolute top-1 right-1 z-10 flex items-center gap-0.5 opacity-0 transition-opacity duration-150 ease-out group-hover/codeblock:opacity-100 focus-within:opacity-100 compact:opacity-100"
  >
    <IconButton label={codeWrapLabel(unwrapped)} size="sm" onClick={toggleWrap}>
      {#snippet children()}
        <Icon icon={TextWrap} size={13} />
      {/snippet}
    </IconButton>
    <CopyButton
      text={token.text}
      label="Copy code"
      onError={() => addToast('error', 'Failed to copy')}
    />
  </div>
</div>
