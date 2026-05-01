<script module lang="ts">
  let nextMarkdownInstanceID = 0;
</script>

<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { disposeMarkdownEnhancements, enhanceMarkdown } from '../../utils/markdownEnhance';
  import { renderMarkdown } from '../../utils/markdownRender';

  let {
    source,
    streaming = false,
    workspacePath = '',
    class: className = '',
  }: {
    source: string;
    streaming?: boolean;
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in the rendered text. Pass `pane.thread.workspacePath`
     *  from per-thread surfaces; non-thread surfaces (design previews,
     *  notebook cells) leave empty and accept that relative-path
     *  click-to-open will surface a clear "requires workspacePath" error. */
    workspacePath?: string;
    class?: string;
  } = $props();

  let root: HTMLDivElement | undefined;
  let generation = 0;
  let renderFrame: number | null = null;
  let html = $state('');
  const renderScope = `markdown-${nextMarkdownInstanceID++}`;

  $effect(() => {
    const currentSource = source;
    const currentStreaming = streaming;
    if (renderFrame !== null) {
      cancelAnimationFrame(renderFrame);
      renderFrame = null;
    }

    const render = () => {
      renderFrame = null;
      html = renderMarkdown(currentSource);
    };

    if (currentStreaming) {
      renderFrame = requestAnimationFrame(render);
    } else {
      render();
    }

    return () => {
      if (renderFrame !== null) {
        cancelAnimationFrame(renderFrame);
        renderFrame = null;
      }
    };
  });

  onDestroy(() => {
    if (renderFrame !== null) {
      cancelAnimationFrame(renderFrame);
    }
    if (root) disposeMarkdownEnhancements(root);
  });

  $effect(() => {
    const currentGeneration = ++generation;
    const currentHtml = html;
    const currentStreaming = streaming;
    // Capture synchronously so the dependency lands on the effect
    // graph; the value is read again in the async `tick().then(...)`
    // body, which runs outside the tracked scope.
    const currentWorkspace = workspacePath;
    const node = root;

    if (!node || !currentHtml) {
      return;
    }

    let disposed = false;
    tick().then(async () => {
      if (disposed || currentGeneration !== generation || !root) {
        return;
      }
      await enhanceMarkdown(root, {
        generation: currentGeneration,
        renderScope,
        streaming: currentStreaming,
        isCurrent: (candidate) => candidate === generation,
        workspacePath: currentWorkspace,
      });
    });

    return () => {
      disposed = true;
      // Unmount the prior generation's CopyButtons before {@html}
      // replaces their host nodes. enhanceMarkdown will also dispose
      // at the start of the next pass — this cleanup additionally
      // covers the unmount path.
      if (root) disposeMarkdownEnhancements(root);
    };
  });

</script>

<div bind:this={root} class={['markdown-body', className].filter(Boolean).join(' ')}>
  {@html html}
</div>
