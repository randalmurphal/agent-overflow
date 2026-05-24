<script lang="ts">
  // Streaming-aware markdown renderer.
  //
  // Powered by `svelte-streamdown`, which mounts a Svelte component tree
  // directly off marked tokens — every paragraph, code block, math block,
  // diagram, etc. is its own keyed Svelte child. The DOM is reactive but
  // node identity is preserved across content updates, so:
  //   - text selection survives streaming chunks
  //   - shiki-highlighted code blocks don't flash back to plain text
  //     between updates
  //   - mermaid SVGs render once and stay mounted
  //   - katex output isn't re-typeset on every chunk
  //
  // Replaces the legacy `marked → DOMPurify → {@html}` wholesale-replace
  // pipeline plus our hand-rolled `enhanceMarkdown` post-processor for
  // shiki / mermaid / katex / copy buttons. The library handles all four
  // natively as opt-in components.
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

  import { getContext } from 'svelte';
  import { Streamdown } from 'svelte-streamdown';
  import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';
  import StreamdownCodeHost from './markdown/StreamdownCodeHost.svelte';
  import StreamdownMermaidHost from './markdown/StreamdownMermaidHost.svelte';
  import StreamdownMathHost from './markdown/StreamdownMathHost.svelte';
  import { chatMarkdownTheme, extraShikiLanguages } from './markdown/streamdownTheme';
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
</script>

<div
  bind:this={root}
  class={['markdown-body', className].filter(Boolean).join(' ')}
>
  <Streamdown
    content={processedSource}
    parseIncompleteMarkdown={streaming}
    baseTheme="tailwind"
    theme={chatMarkdownTheme}
    shikiLanguages={extraShikiLanguages}
    {allowedLinkPrefixes}
    allowedImagePrefixes={['*']}
    renderHtml={false}
    controls={{ code: false, table: false }}
    {extensions}
    onsettled={handleSettled}
    components={{
      code: StreamdownCodeHost,
      mermaid: StreamdownMermaidHost,
      math: StreamdownMathHost,
    }}
  />
</div>
