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
  import { Streamdown, type ProvenAppend } from '../../markdown';
  import {
    CHAT_MARKDOWN_PRESENCE_CONTEXT,
    CHAT_MARKDOWN_SETTLED_CONTEXT,
  } from './markdownSettledContext';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';
  import { resolveMermaidThemeConfig } from './markdown/mermaidTokens';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import {
    STREAMDOWN_STATIC_RENDERERS,
    STREAMDOWN_STATIC_WORK_SCHEDULER,
    streamdownComponentsFor,
  } from './markdown/streamdownConfig';
  import { MarkdownFenceUnwrapper } from './markdown/unwrapMarkdownFence';
  import {
    ensureMarkdownCopyDelegate,
    ensurePathLinkClickDelegate,
  } from '../../utils/markdownEnhance';
  import {
    LOCAL_IMAGE_HREF_PREFIX,
    PATH_LINK_HREF_PREFIX,
    buildPathLinkExtension,
  } from '../../utils/pathLinkExtension';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import StreamdownImageHost from './markdown/StreamdownImageHost.svelte';
  import {
    captureStreamingAssistantSelection,
    restoreStreamingAssistantSelection,
    type StreamingAssistantSelectionSnapshot,
  } from './markdown/streamingAssistantSelection';
  import { ensureStaticCodeCopyDelegate } from './markdown/staticCodeBlock';
  import { registerFootnoteSource } from './markdown/footnoteDefinitions';
  import type { PathRef } from '../../types/models';
  import { StreamingBoundarySplitter } from '../../markdown/boundary';
  import { getSettings } from '../../stores/settings.svelte';
  import { isViewOnlySession } from '../../transport/runMode';
  import { isHarnessSession } from '../../transport/harnessMode';

  let {
    source,
    sourceAppend,
    streaming = false,
    workspacePath = '',
    pathRefs,
    class: className = '',
  }: {
    source: string;
    /** Opaque proof that source extends the previous source. */
    sourceAppend?: ProvenAppend;
    streaming?: boolean;
    /** Absolute base directory the path-link pipeline resolves against.
     *  Pass `pane.thread.workspacePath` from per-thread surfaces. It
     *  gates BOTH halves of the extension: relative prose paths resolve
     *  against it at click time, and markdown-link href rewriting
     *  (`[x](/abs/file.md)` → editor affordance) requires it for every
     *  shape — a surface with no workspace (PR bodies, review comments,
     *  design previews) gets no href rewriting at all, so third-party
     *  root-relative links can never become editor links there. */
    workspacePath?: string;
    /** Server-validated allowlist of file paths to linkify in prose.
     *  Only paths in this list get the prose `agent-overflow:` link
     *  treatment — this component never invents prose links. Choosing
     *  a value on a new surface: `undefined` disables prose
     *  linkification, while a present `workspacePath` still enables
     *  explicit local-link and local-image href rewriting. `[]` has the
     *  same behavior; use EMPTY_PATH_REFS rather than a fresh literal. */
    pathRefs?: PathRef[];
    class?: string;
  } = $props();

  let viewOnly = $derived(isViewOnlySession());
  const diagnostics = isHarnessSession();

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

  // Install document-level delegates once per page lifetime. Subsequent
  // ChatMarkdown mounts are no-ops; subsequent calls from other surfaces
  // share the same listeners.
  $effect(() => {
    ensureMarkdownCopyDelegate();
    ensurePathLinkClickDelegate();
    ensureStaticCodeCopyDelegate();
  });

  // Marked inline extension derived from the validated allowlist. The
  // extension is rebuilt when `pathRefs` / `workspacePath` change. A
  // missing allowlist disables prose linkification only; explicit local
  // hrefs still normalize whenever the surface has a workspace. The shared
  // empty array keeps that fallback identity stable across streaming frames.
  // buildPathLinkExtension returns undefined when both halves are inert.
  const pathLinkExtension = $derived(
    viewOnly
      ? undefined
      : buildPathLinkExtension(pathRefs ?? EMPTY_PATH_REFS, workspacePath),
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
  const allowedImagePrefixes = ['*', LOCAL_IMAGE_HREF_PREFIX];

  // LOAD-BEARING ABSENCE: no `defaultOrigin` is ever passed to
  // Streamdown. With no origin, streamdown's `parseUrl` returns null
  // for every schemeless href, so the `*` wildcard can never resurrect
  // a raw relative anchor (url.js's `inputWasRelative` return).
  // Passing a defaultOrigin here would reopen origin-isolation defect
  // A (docs/specs/remote-access-boundaries.md) through transformUrl
  // itself, bypassing the vendored Link/Image fixes.

  // Diagram palette. Without a `mermaidConfig` the vendored Streamdown
  // falls back to mermaid's built-in `'dark'`/`'default'` themes, which
  // are the only colors in the app that come from nowhere near the token
  // layer. `markdown/mermaidTokens.ts` resolves our tokens to concrete
  // sRGB and pins `theme: 'base'` so mermaid derives everything from
  // them. Streamdown's context exposes this to the vendored
  // `Mermaid.svelte`; ChatMarkdown owns it because the CONTEXT is
  // created here — `StreamdownMermaidHost` is a child of it and has no
  // door back up.
  //
  // Deliberately a `$derived`, not an `$effect`: evaluation is pulled by
  // the first read, which is mermaid's own render (after its async
  // `import('mermaid')` resolves). The document class it reads against is
  // stamped by App.svelte's `$effect.pre`, a root render effect that runs
  // ahead of every descendant user effect in the flush.
  //
  // This invalidates on EVERY settings write (`getResolvedTheme` and the
  // font read inside the resolver both go through the wholesale-replaced
  // settings object). That is fine and load-bearing: the resolver's memo
  // returns the SAME object for an unchanged palette identity, which is
  // what stops the vendored `Mermaid.svelte`'s `{@attach}` from
  // re-rendering every visible diagram on an unrelated save.
  const mermaidConfig = $derived(resolveMermaidThemeConfig(getResolvedTheme()));

  const markdownFenceUnwrapper = new MarkdownFenceUnwrapper();
  const processedSource = $derived(
    markdownFenceUnwrapper.render(
      source,
      streaming,
      sourceAppend,
    ),
  );
  const processedSourceAppend = $derived.by(() => {
    processedSource;
    return markdownFenceUnwrapper.outputAppend;
  });

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
      return { prefix: processedSource, tail: '', tailAppend: undefined };
    }
    const split = boundarySplitter.split(
      processedSource,
      processedSourceAppend,
    );
    return {
      ...split,
      tailAppend: boundarySplitter.tailAppend,
    };
  });

  // A stable block migrates from the volatile Streamdown to the committed
  // owner. Both render identical DOM, but they cannot share node identity.
  // Capture against the stable message root before Svelte replaces that
  // subtree and restore after the flush, so a reader selecting the completed
  // block does not lose the range when the next block starts.
  let markdownRoot: HTMLElement;
  let renderedPrefix = '';
  let boundarySelectionGeneration = 0;
  $effect.pre(() => {
    const { prefix } = splitDerived;
    if (prefix === renderedPrefix) return;
    const generation = ++boundarySelectionGeneration;
    const selection: StreamingAssistantSelectionSnapshot | null = markdownRoot
      ? captureStreamingAssistantSelection(markdownRoot)
      : null;
    renderedPrefix = prefix;
    if (!selection) return;
    // The first microtask follows Svelte's scheduled flush. The second follows
    // every descendant render effect in that flush, matching the direct-tail
    // reset path that uses the same selection primitive.
    queueMicrotask(() => queueMicrotask(() => {
      if (
        generation === boundarySelectionGeneration &&
        selection.root.isConnected &&
        selection.root === markdownRoot
      ) restoreStreamingAssistantSelection(selection);
    }));
  });

  // Publish the source this surface renders, so a footnote chip's popup can
  // resolve `[^1]` to its `[^1]: body` definition. The body renders nowhere
  // (the parser drops the definition block) and the ref token's own
  // back-reference is empty across block boundaries, so the SOURCE is the
  // only answer — see `markdown/footnoteDefinitions.ts`. An attachment
  // rather than an `$effect`, because the registration is per NODE and must
  // not make the root element a reactive dependency of anything: the
  // selection-preserving `$effect.pre` above reads `markdownRoot` and
  // re-running it on bind would be a live hazard. The reader closes over
  // `processedSource` lazily, so a streaming delta re-registers nothing.
  const publishFootnoteSource = (node: HTMLElement) =>
    registerFootnoteSource(node, () => processedSource);

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
  const showVolatileTail = $derived(
    !hideVolatileTail && (splitDerived.tail.length > 0 || splitDerived.prefix.length === 0),
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
  root div. The committed instance also stamps its already-rendered last outer
  block type there, so app.css can pair it with the volatile root's direct
  first `<p>` and re-establish a paragraph→paragraph gap without a
  descendant-sensitive `:has()` selector.
  The two instances are separate
  containers, so the adjacent-sibling `p + p` spacing rule can't match
  across them and a p→p gap (paragraphs are `margin: 0`) would otherwise
  collapse until the next block commits. Non-paragraph seams need no rule —
  their blocks' intrinsic margins collapse across the (plain) container
  boundary to the correct gap on their own. A settled (non-streaming)
  message renders as a single `md-committed` container and never matches
  the seam rule.
-->
{#snippet streamdownInstance(
  content: string,
  parseIncompleteMarkdown: boolean,
  wrapperClass: string,
  contentAppend?: ProvenAppend,
  trimFirstBlockMargin = false,
  trimLastBlockMargin = false,
)}
  <Streamdown
    class={wrapperClass}
    {content}
    {contentAppend}
    {parseIncompleteMarkdown}
    isolatedVolatileTail={parseIncompleteMarkdown}
    {diagnostics}
    theme={chatMarkdownTheme}
    {mermaidConfig}
    {allowedLinkPrefixes}
    {allowedImagePrefixes}
    renderHtml={false}
    compactStaticHtml={true}
    {trimFirstBlockMargin}
    {trimLastBlockMargin}
    staticRenderers={STREAMDOWN_STATIC_RENDERERS}
    staticWorkScheduler={STREAMDOWN_STATIC_WORK_SCHEDULER}
    {extensions}
    onsettled={handleSettled}
    components={streamdownComponentsFor(parseIncompleteMarkdown)}
  >
    {#snippet inlineCitation({ token })}
      {token.text ?? token.raw}
    {/snippet}
    {#snippet image({ token })}
      <StreamdownImageHost {token} />
    {/snippet}
  </Streamdown>
{/snippet}

<div
  bind:this={markdownRoot}
  {@attach publishFootnoteSource}
  class={['markdown-body', className].filter(Boolean).join(' ')}
>
  {#if splitDerived.prefix}
    {@render streamdownInstance(
      splitDerived.prefix,
      false,
      'md-committed',
      undefined,
      true,
      !showVolatileTail,
    )}
  {/if}
  {#if showVolatileTail}
    {@render streamdownInstance(
      splitDerived.tail,
      streaming,
      'md-volatile',
      splitDerived.tailAppend,
      !splitDerived.prefix,
      true,
    )}
  {/if}
</div>
