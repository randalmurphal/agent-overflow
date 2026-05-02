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
  // What we still own as a post-process pass:
  //   - **Path linkification** — wrapping `src/lib/foo.ts:42` style
  //     paths that appear in PROSE TEXT (not as markdown links) with
  //     editor-open anchors. The library doesn't know about our
  //     project-relative path resolution, and walking text nodes after
  //     render is the simplest correct path. Skipped while streaming
  //     for the same reason as before — the source is moving and
  //     re-walking on every chunk would burn cycles.
  //   - **Markdown-aware copy** — the document-level `copy` delegate
  //     reads `.markdown-body` and serializes the selected range back
  //     to markdown. Outer wrapper still carries that class.

  import { Streamdown } from 'svelte-streamdown';
  import StreamdownCodeHost from './markdown/StreamdownCodeHost.svelte';
  import StreamdownMermaidHost from './markdown/StreamdownMermaidHost.svelte';
  import StreamdownMathHost from './markdown/StreamdownMathHost.svelte';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';
  import { enhancePathLinks, ensureMarkdownCopyDelegate } from '../../utils/markdownEnhance';

  let {
    source,
    streaming = false,
    workspacePath = '',
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
    class?: string;
  } = $props();

  let root: HTMLDivElement | undefined = $state();

  // Install the markdown-aware copy delegate once per page lifetime.
  // Lives on document; subsequent ChatMarkdown mounts are no-ops here.
  $effect(() => {
    ensureMarkdownCopyDelegate();
  });

  // Path-link enrichment runs after Streamdown has settled the DOM for
  // a given source. Skipped during streaming because (a) text nodes are
  // mutating and walking them mid-stream would re-linkify partial
  // paths repeatedly, and (b) the user can't click yet anyway. Once
  // streaming flips off, the source is settled and we walk the rendered
  // tree once. Subsequent source changes (rare on a completed item)
  // re-run the walker; the linkifier skips already-converted anchors.
  $effect(() => {
    void source;
    if (streaming) return;
    if (!root) return;
    enhancePathLinks(root, workspacePath);
  });
</script>

<div
  bind:this={root}
  class={['markdown-body', className].filter(Boolean).join(' ')}
>
  <Streamdown
    content={source}
    parseIncompleteMarkdown={streaming}
    baseTheme="tailwind"
    theme={chatMarkdownTheme}
    allowedLinkPrefixes={['*']}
    allowedImagePrefixes={['*']}
    renderHtml={false}
    controls={{ code: false, table: false }}
    components={{
      code: StreamdownCodeHost,
      mermaid: StreamdownMermaidHost,
      math: StreamdownMathHost,
    }}
  />
</div>
