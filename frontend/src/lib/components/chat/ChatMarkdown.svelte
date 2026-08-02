<script lang="ts">
  // Streaming-aware markdown renderer.
  //
  // Powered by `svelte-streamdown`, which mounts a Svelte component tree
  // directly off marked tokens — every paragraph, code block, math block,
  // diagram, etc. is its own keyed Svelte child. The DOM is reactive but
  // node identity is preserved across content updates, so:
  //   - text selection survives streaming chunks
  //   - highlighted code blocks don't flash back to plain text
  //     between updates
  //   - mermaid SVGs render once and stay mounted
  //   - katex output isn't re-typeset on every chunk
  //
  // Replaces the legacy `marked → DOMPurify → {@html}` wholesale-replace
  // pipeline plus our hand-rolled `enhanceMarkdown` post-processor for
  // highlight / mermaid / katex / copy buttons. The library handles all
  // four natively as opt-in components.
  //
  // **Path linkification** is now part of the initial markdown parse:
  // `pathLinkExtension.ts` builds a marked inline extension that turns
  // each server-validated path in `pathRefs` into a real markdown `link`
  // token. By the time the DOM exists, the anchor is already settled —
  // no post-render walker, no visible "shift" when streaming ends. A
  // document-level click delegate (installed by markdownEnhance.ts)
  // intercepts clicks on `agent-overflow:open?path=…` hrefs and forwards
  // to the `OpenInEditor` binding.
  //
  // **Markdown-aware copy** still runs through the document-level copy
  // delegate, reading `.markdown-body` and serializing the selected
  // range back to markdown. Outer wrapper still carries that class.
  // The clipboard also gets a `text/html` flavor built from that same
  // markdown (`utils/markdownClipboard.ts`), so a paste into a rich
  // target keeps the structure this component renders.

  import { getContext } from 'svelte';
  import { Streamdown } from 'svelte-streamdown';
  import {
    CHAT_MARKDOWN_PRESENCE_CONTEXT,
    CHAT_MARKDOWN_SETTLED_CONTEXT,
  } from './markdownSettledContext';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';
  import {
    STREAMDOWN_ALLOWED_IMAGE_PREFIXES,
    STREAMDOWN_CONTROLS,
    streamdownComponentsFor,
  } from './markdown/streamdownConfig';
  import { unwrapMarkdownFence } from './markdown/unwrapMarkdownFence';
  import {
    ensureMarkdownCopyDelegate,
    ensurePathLinkClickDelegate,
  } from '../../utils/markdownEnhance';
  import {
    PATH_LINK_HREF_PREFIX,
    buildPathLinkExtension,
  } from '../../utils/pathLinkExtension';
  import type { PathRef } from '../../types/models';
  import { StreamingBoundarySplitter } from '../../markdown/boundary';
  import { getSettings } from '../../stores/settings.svelte';

  let {
    source,
    streaming = false,
    workspacePath = '',
    pathRefs,
    class: className = '',
  }: {
    source: string;
    streaming?: boolean;
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in prose text. Pass `pane.thread.workspacePath`
     *  from per-thread surfaces; non-thread surfaces (design previews,
     *  notebook cells) leave empty and accept that relative-path click-
     *  to-open will surface a clear "requires workspacePath" error. */
    workspacePath?: string;
    /** Server-validated allowlist of file paths to linkify in prose.
     *  When defined, only paths in this list get the `agent-overflow:`
     *  link treatment. Pass `[]` (not `undefined`) on surfaces that
     *  haven't been wired to a validation pipeline yet, so the marked
     *  extension is skipped entirely rather than fabricating links. */
    pathRefs?: PathRef[];
    class?: string;
  } = $props();

  // Aggregation hook for the chat warm-gate "is the visible
  // async-typesetting context settled?" signal. MessageTimeline
  // setContext()s a one-shot mark function on every armWarmup() so
  // every ChatMarkdown rendered inside the timeline tree contributes
  // to the same aggregation boolean. Non-timeline ChatMarkdowns
  // (settings preview, design canvas, etc.) get `undefined` here and
  // simply skip the signal.
  const markSettled = getContext<(() => void) | undefined>(
    CHAT_MARKDOWN_SETTLED_CONTEXT,
  );
  const handleSettled = (): void => {
    markSettled?.();
  };

  // Live presence registration for the warm gate: while at least one
  // ChatMarkdown is mounted in the timeline, async typesetting may still
  // be coming and the settled signal must be earned via `onsettled`;
  // with none mounted the timeline reports settled-by-absence. The
  // $effect cleanup unregisters on unmount.
  const registerPresence = getContext<(() => () => void) | undefined>(
    CHAT_MARKDOWN_PRESENCE_CONTEXT,
  );
  $effect(() => registerPresence?.());

  let root: HTMLDivElement | undefined = $state();

  // Install document-level delegates once per page lifetime. Subsequent
  // ChatMarkdown mounts are no-ops; subsequent calls from other surfaces
  // share the same listeners.
  $effect(() => {
    ensureMarkdownCopyDelegate();
    ensurePathLinkClickDelegate();
  });

  // Marked inline extension derived from the validated allowlist. The
  // extension is rebuilt on every change to `pathRefs` / `workspacePath`
  // — both should be stable across streaming chunks, so the rebuild
  // cost is negligible. When the allowlist is empty (or undefined), the
  // primitive returns undefined and we pass no extensions array, leaving
  // Streamdown to render unenriched markdown.
  const pathLinkExtension = $derived(
    pathRefs && pathRefs.length > 0
      ? buildPathLinkExtension(pathRefs, workspacePath)
      : undefined,
  );
  const extensions = $derived(pathLinkExtension ? [pathLinkExtension] : undefined);

  // The path-link prefix carries a per-page-load nonce so only links
  // emitted by our marked extension pass Streamdown's `transformUrl`
  // gate. The default `*` sentinel only scopes http/https URLs (see
  // streamdown's url.js); custom schemes need an exact prefix match
  // on the canonical href. Agent prose like
  // `[click](agent-overflow:open?path=/etc/passwd)` cannot satisfy
  // the nonce-prefixed form and is rejected before any anchor is
  // rendered.
  const allowedLinkPrefixes = ['*', PATH_LINK_HREF_PREFIX];

  const processedSource = $derived(unwrapMarkdownFence(source));

  // Streaming-only block-boundary memoization. When `streaming === true`,
  // the source is split at the last stable markdown block boundary
  // (incremark's BoundaryDetector — vendored under
  // `lib/markdown/boundary/`); the committed prefix is parsed once and
  // never re-parses while streaming, and the volatile tail re-parses on
  // every update with `parseIncompleteMarkdown` enabled to absorb
  // half-typed fences / tables / setext underlines.
  //
  // `StreamingBoundarySplitter` owns the committed high-water mark and a
  // persistent detector, resuming detection from the last committed line
  // each call (O(n) over the stream) instead of re-scanning from line 0
  // every reveal tick (O(n²)). It is created once per component instance
  // and is non-reactive; `split()` is idempotent for a given source, so
  // calling it inside the derivation is safe even if Svelte coalesces or
  // re-evaluates. `streaming` is only ever true for the assistant_text
  // row, whose source is append-only — the splitter's precondition.
  const boundarySplitter = new StreamingBoundarySplitter();

  const splitDerived = $derived.by(() => {
    if (!streaming) {
      return { prefix: processedSource, tail: '' };
    }
    return boundarySplitter.split(processedSource);
  });

  // "Streaming enabled" (Settings → Live Updates) governs whether the
  // in-progress markdown block is shown while a turn streams. When it is
  // off, the volatile tail is withheld: the row reveals one committed
  // block at a time, each appearing only once it stabilises at a markdown
  // boundary — the user opted out of word-by-word streaming. This is
  // orthogonal to low-power mode, which only strips the reveal ANIMATION
  // (see threadStreamingReveal.svelte.ts's revealImmediately) while still
  // showing the live tail. Gated behind `streaming` so settled rows and
  // non-streaming surfaces short-circuit before reading the setting — no
  // reactive dependency, always rendered in full. On completion
  // `streaming` flips false and the whole message renders as committed.
  const hideVolatileTail = $derived(
    streaming && !getSettings().streamingEnabled,
  );
</script>

<!--
  Both Streamdown instances share identical theme / extensions / safety
  config; only `content` and `parseIncompleteMarkdown` differ between
  the committed prefix (parsed once, completed) and the volatile tail
  (re-parses with incomplete-markdown auto-close while streaming).
  The snippet keeps the props in one place so a future tweak (theme,
  link allowlist, host component) can't silently miss one half.

  The volatile tail uses deferred hosts for math and mermaid: KaTeX and
  mermaid render only on the committed prefix, so a half-typed matrix /
  diagram in the tail never enters typesetting mid-chunk. Without this,
  per-chunk KaTeX re-renders produced contentRO height deltas the
  stick-to-bottom spring chased across the viewport. Code and prose
  still stream live (code blocks keep their throttled backend-span
  path, prose keeps incremental markdown).

  `wrapperClass` (`md-committed` / `md-volatile`) marks each Streamdown's
  root div so the `:has()`-gated seam rule in app.css can re-establish the
  gap at a paragraph→paragraph seam: the two instances are separate
  containers, so the adjacent-sibling `p + p` spacing rule can't match
  across them and a p→p gap (paragraphs are `margin: 0`) would otherwise
  collapse until the next block commits. Non-paragraph seams need no rule —
  their blocks' intrinsic margins collapse across the (plain) container
  boundary to the correct gap on their own. A settled (non-streaming)
  message renders as a single `md-committed` container and never matches
  the seam rule.
-->
{#snippet streamdownInstance(content: string, parseIncompleteMarkdown: boolean, wrapperClass: string)}
  <Streamdown
    class={wrapperClass}
    {content}
    {parseIncompleteMarkdown}
    baseTheme="tailwind"
    theme={chatMarkdownTheme}
    {allowedLinkPrefixes}
    allowedImagePrefixes={STREAMDOWN_ALLOWED_IMAGE_PREFIXES}
    renderHtml={false}
    controls={STREAMDOWN_CONTROLS}
    {extensions}
    onsettled={handleSettled}
    components={streamdownComponentsFor(parseIncompleteMarkdown)}
  >
    {#snippet inlineCitation({ token })}
      {token.text ?? token.raw}
    {/snippet}
  </Streamdown>
{/snippet}

<div
  bind:this={root}
  class={['markdown-body', className].filter(Boolean).join(' ')}
>
  {#if splitDerived.prefix}
    {@render streamdownInstance(splitDerived.prefix, false, 'md-committed')}
  {/if}
  {#if !hideVolatileTail && (splitDerived.tail || !splitDerived.prefix)}
    {@render streamdownInstance(splitDerived.tail, streaming, 'md-volatile')}
  {/if}
</div>
