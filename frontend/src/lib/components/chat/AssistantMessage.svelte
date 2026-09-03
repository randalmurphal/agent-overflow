<script lang="ts">
  import { onDestroy } from 'svelte';
  import type { Item } from '../../types/models';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import type {
    PaneSession,
    RevealRead,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { EMPTY_PATH_REFS, getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { formatTimeOfDay } from '../../utils/format';
  import { getSettings } from '../../stores/settings.svelte';
  import { copyMarkdownToClipboard } from '../../utils/markdownClipboard';
  import { ingestPersistedCodeSpans } from '../../utils/persistedSpans';
  import { splitAtBoundary } from '../../markdown/boundary/split';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { RawJsonFenceFormatter } from './markdown/rawJsonFence';
  import {
    AllowlistedPathCompletionGuard,
    createStreamingAssistantLiteralOwner,
  } from './markdown/streamingAssistantLiteralOwner';
  import type { StreamingAssistantRenderContext } from '../../stores/streamingAssistantReveal';
  import { previewRewriteKey } from '../../stores/devServers.svelte';
  import { hasScope } from '../../transport/scopes';
  import { isHarnessSession } from '../../transport/harnessMode';

  type AssistantMarkdownForensics = {
    readonly itemId: string;
    readonly canonicalSource: string;
    readonly parserSource: string;
    readonly renderedSource: string;
    readonly streaming: boolean;
  };

  type ForensicAssistantBody = HTMLElement & {
    __aoMarkdownForensics?: AssistantMarkdownForensics;
  };

  let { pane, item }: { pane?: PaneSession & RevealRead & RowUiRegistry & ScrollHost; item: Item } = $props();

  // A schema-bound turn answers with one unfenced JSON document, often a
  // single line tens of KB long; as prose that line re-pairs its `_` and
  // backtick characters on every reveal tick and restyles text already
  // on screen (the tail-of-stream flash, 2026-08-22). The formatter
  // wraps it as a pretty-printed ```json fence instead, prefix-stable so
  // the code host's incremental rendering stays incremental; anything
  // that is not JSON comes back untouched. One instance per row because
  // it resumes from the previous source. See `markdown/rawJsonFence.ts`.
  const jsonFence = new RawJsonFenceFormatter();

  // Warm the code span cache from the row's persisted fence spans
  // (items.meta `codeSpans`, written at settle). The init call is the
  // cold-mount path: it seeds synchronously (tables memoized) BEFORE
  // ChatMarkdown's code hosts mount, so their first cache reads hit and
  // no highlight RPC fires. The initial-value capture is the point —
  // the $effect below covers meta arriving later (settle mid-session,
  // virtualized item replacement). Streaming rows have no codeSpans key
  // yet and no-op cheaply.
  // svelte-ignore state_referenced_locally
  ingestPersistedCodeSpans(item.meta);
  $effect(() => {
    ingestPersistedCodeSpans(item.meta);
  });

  // Rendered streaming mode. Status alone is NOT the signal: the wire
  // settles `status` to a terminal value while the per-item smoother is
  // still draining the reveal (often for seconds on a long final
  // message), and flipping ChatMarkdown to settled mode mid-drain drops
  // the volatile-tail split and its incomplete-markdown guards while the
  // text is still visibly growing — a half-arrived nested bullet then
  // parses as a setext underline and balloons the line above into an
  // <h2> for a frame (the end-of-turn per-line reflow glitch). Hold
  // streaming rendering until the smoother disposes (reactive via the
  // pane's SvelteMap-backed `isItemSmoothing`); panes are absent on
  // non-timeline surfaces, which only ever render settled items.
  const streaming = $derived(
    item.status === 'streaming' || (pane?.isItemSmoothing(item.id) ?? false),
  );
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // /\S/.test short-circuits at the first non-whitespace character — for
  // long streaming summaries this matters; .trim() would allocate the
  // full string every reactive tick.
  const canCopy = $derived(!streaming && /\S/.test(item.summary));

  // The timestamp/copy row is a completion artifact — it must not float
  // above an EMPTY body. With live updates disabled the body is withheld
  // until the first markdown block commits (ChatMarkdown hides the
  // volatile tail), so an unconditional meta row shows a lone timestamp
  // the instant streaming starts, before any text appears. Gate the row
  // on "is there visible body content yet?".
  //
  // Computed synchronously at mount so settled rows (and streaming rows
  // scrolled back into view with content already committed) show their
  // meta immediately — a deferred $effect would blank the timestamp for
  // one frame on every virtualized remount. The $effect then only latches
  // the false→true transition as content first commits mid-stream; once
  // latched it never re-splits (summary is append-only), so the boundary
  // scan runs only during the brief pre-commit window, never over the
  // whole settled message every tick.
  function bodyIsVisible(): boolean {
    if (!/\S/.test(item.summary)) return false;
    // Settled rows and live-streaming rows reveal as soon as any
    // non-whitespace arrives; only the streaming-disabled path withholds
    // the tail and needs a committed block before anything is on screen.
    if (!streaming || getSettings().streamingEnabled) return true;
    return /\S/.test(splitAtBoundary(item.summary).prefix);
  }
  let hasVisibleBody = $state(bodyIsVisible());
  $effect(() => {
    if (!hasVisibleBody && bodyIsVisible()) hasVisibleBody = true;
  });

  // Path-link allowlist for assistant prose. Triage validates paths on
  // every streaming text flush and pushes the resulting `pathRefs` onto
  // `Item.meta` mid-stream (`internal/triage` → `applyItemMeta`), with a
  // final authoritative set on settle; here we surface it to
  // ChatMarkdown so only Go-validated paths get wrapped. Falling back
  // to `[]` (not `undefined`) for rows without pathRefs is
  // intentional: pre-pathlinks history rows render plain rather than
  // falling through to the local regex, which is the bug we're fixing.
  // `getPathRefsFromMeta` is memoized on the meta string, so this
  // derived keeps a stable array identity across the per-frame item
  // replacements of a streaming row — a fresh identity per frame would
  // rebuild ChatMarkdown's marked extension and re-lex every block.
  const pathRefs = $derived(getPathRefsFromMeta(item.meta) ?? EMPTY_PATH_REFS);
  const workspacePath = $derived(paneWorkspacePath(pane));
  const threadId = $derived(pane?.threadId ?? '');
  // Stable across per-reveal row replacements. A context transition can
  // remount or reinterpret the volatile parser tree even when its source is
  // unchanged, so the router must drop direct DOM before that transition.
  let cachedParserRenderContext: StreamingAssistantRenderContext | undefined;
  const parserRenderContext = $derived.by(() => {
    const nextStreaming = streaming;
    const nextVolatileTailVisible = !nextStreaming || getSettings().streamingEnabled;
    const nextPathLinksInert = !hasScope('host');
    const nextWorkspacePath = workspacePath;
    const nextPreviewKey = previewRewriteKey(threadId);
    if (
      cachedParserRenderContext?.streaming === nextStreaming &&
      cachedParserRenderContext.volatileTailVisible === nextVolatileTailVisible &&
      cachedParserRenderContext.pathLinksInert === nextPathLinksInert &&
      cachedParserRenderContext.workspacePath === nextWorkspacePath &&
      cachedParserRenderContext.previewKey === nextPreviewKey
    ) {
      return cachedParserRenderContext;
    }
    cachedParserRenderContext = {
      streaming: nextStreaming,
      volatileTailVisible: nextVolatileTailVisible,
      pathLinksInert: nextPathLinksInert,
      workspacePath: nextWorkspacePath,
      previewKey: nextPreviewKey,
    };
    return cachedParserRenderContext;
  });

  // Codex marks an assistant message the model emitted mid-turn — a progress
  // note alongside its work, not the turn's answer — with `delivery: "async"`
  // on the block's stop event. Read POSITIVELY and only positively: absence is
  // not evidence of finality (most providers never set the key at all), so a
  // row without it is rendered exactly as before.
  const isInterim = $derived(
    (parseJsonObject(item.meta)?.delivery ?? '') === 'async',
  );

  // Streamdown reads the router's stable parser checkpoint while sink-owned
  // literal text extends its trailing leaf. Punctuation and authoritative
  // rewrites clear the checkpoint and hand the canonical source back to it.
  const parserSource = $derived(
    pane?.assistantMarkdownParserSource?.(
      item.id,
      item.summary,
      parserRenderContext,
    ) ?? item.summary,
  );
  // The pane proves append lineage at the smoother write chokepoint. Feeding
  // the emitted delta to the JSON formatter avoids startsWith/charCodeAt on
  // the growing cons string, which would flatten and copy the full answer.
  const parserSourceAppend = $derived(
    pane?.assistantMarkdownSourceAppend?.(item.id, parserSource),
  );
  const markdownSource = $derived(
    jsonFence.render(parserSource, streaming, parserSourceAppend),
  );
  const markdownSourceAppend = $derived.by(() => {
    markdownSource;
    return jsonFence.outputAppend;
  });
  const directSourceIsRawJson = $derived.by(() => {
    markdownSource;
    return jsonFence.sourceIsRawJson;
  });
  const pathCompletionGuard = new AllowlistedPathCompletionGuard();
  let bodyRoot: HTMLElement;
  const directRevealSink = createStreamingAssistantLiteralOwner({
    getRoot: () => bodyRoot,
    canAppendSource: (source, nextSource, delta) =>
      !directSourceIsRawJson &&
      !pathCompletionGuard.completes(pathRefs, source, nextSource, delta),
  });

  // Harness-only, source-preserving visibility for transient Markdown
  // failures. The object stores no source strings: its getters read the row
  // and parser state that already own them. A CDP probe can therefore compare
  // one painted DOM frame with the exact canonical/parser sources without a
  // full-source data attribute or a second retained answer. Ordinary sessions
  // install no property and allocate no object.
  $effect(() => {
    if (!isHarnessSession()) return;
    const root = bodyRoot as ForensicAssistantBody;
    const forensics: AssistantMarkdownForensics = {
      get itemId() { return item.id; },
      get canonicalSource() { return item.summary; },
      get parserSource() { return parserSource; },
      get renderedSource() { return markdownSource; },
      get streaming() { return streaming; },
    };
    Object.defineProperty(root, '__aoMarkdownForensics', {
      configurable: true,
      value: forensics,
    });
    return () => {
      if (root.__aoMarkdownForensics === forensics) {
        delete root.__aoMarkdownForensics;
      }
    };
  });

  // The timeline replaces the item object on every authoritative reveal.
  // Cut that churn off at the primitive id so registration changes only when
  // the row or pane changes. Cleanup lives in onDestroy rather than the effect
  // return because the registration itself owns the transition.
  const revealItemId = $derived(item.id);
  let registeredRevealPane: RevealRead | undefined;
  let registeredRevealItemId = '';
  let registeredRevealGeneration = -1;
  let unregisterRevealSink: (() => void) | undefined;
  $effect(() => {
    const nextPane = pane;
    const nextItemId = revealItemId;
    const nextGeneration = nextPane?.assistantRevealRegistrationGeneration ?? 0;
    if (
      nextPane === registeredRevealPane &&
      nextItemId === registeredRevealItemId &&
      nextGeneration === registeredRevealGeneration
    ) return;

    const releasePrevious = unregisterRevealSink;
    unregisterRevealSink = undefined;
    registeredRevealPane = undefined;
    registeredRevealItemId = '';
    registeredRevealGeneration = -1;
    const transitionErrors: unknown[] = [];
    try {
      releasePrevious?.();
    } catch (error) {
      transitionErrors.push(error);
    }

    try {
      const releaseNext = nextPane?.registerAssistantRevealSink?.(
        nextItemId,
        directRevealSink,
      );
      registeredRevealPane = nextPane;
      registeredRevealItemId = nextItemId;
      registeredRevealGeneration = nextGeneration;
      unregisterRevealSink = releaseNext;
    } catch (error) {
      transitionErrors.push(error);
    }
    if (transitionErrors.length === 1) throw transitionErrors[0];
    if (transitionErrors.length > 1) {
      throw new AggregateError(
        transitionErrors,
        `assistant reveal sink transition failed for ${nextItemId}`,
      );
    }
  });
  onDestroy(() => {
    const release = unregisterRevealSink;
    unregisterRevealSink = undefined;
    registeredRevealPane = undefined;
    registeredRevealItemId = '';
    registeredRevealGeneration = -1;
    release?.();
  });
</script>

<div class="group" data-item-kind={item.kind}>
  <div
    bind:this={bodyRoot}
    class="text-fg-muted"
    data-testid="assistant-message-body"
    data-render-mode="client-markdown"
  >
    <ChatMarkdown
      source={markdownSource}
      sourceAppend={markdownSourceAppend}
      {streaming}
      {workspacePath}
      {pathRefs}
      {threadId}
    />
  </div>
  {#if hasVisibleBody}
    <div
      class="mt-1.5 flex items-center gap-1.5 text-[0.625rem] text-fg-hint"
      data-testid="assistant-message-meta"
    >
      <time class="tabular-nums" datetime={isoTime}>
        {time}
      </time>
      {#if isInterim}
        <span
          class="rounded-full border border-border px-1.5 py-px text-[0.5625rem] uppercase tracking-wide text-fg-hint"
          data-testid="assistant-message-interim"
          title="Sent while the model kept working; not the turn's final answer"
        >
          Interim
        </span>
      {/if}
      <span
        data-testid="assistant-message-copy-slot"
        class="flex h-7 w-7 shrink-0 items-center justify-center"
      >
        {#if canCopy}
          <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
            <CopyButton
              text={item.summary}
              write={copyMarkdownToClipboard}
              label="Copy message"
              onError={() => addToast('error', 'Failed to copy')}
            />
          </span>
        {/if}
      </span>
    </div>
  {/if}
</div>
