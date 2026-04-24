<script module lang="ts">
  let nextMarkdownInstanceID = 0;
</script>

<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { enhanceMarkdown } from '../../utils/markdownEnhance';
  import { renderMarkdown } from '../../utils/markdownRender';

  let {
    source,
    streaming = false,
    class: className = '',
  }: {
    source: string;
    streaming?: boolean;
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
  });

  $effect(() => {
    const currentGeneration = ++generation;
    const currentHtml = html;
    const currentStreaming = streaming;
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
      });
    });

    return () => {
      disposed = true;
    };
  });

</script>

<div bind:this={root} class={['markdown-body', className].filter(Boolean).join(' ')}>
  {@html html}
</div>
