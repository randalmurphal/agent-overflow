<script lang="ts">
  import { tick } from 'svelte';
  import { createStreamingAssistantDomSink } from './streamingAssistantDomSink';
  import type { StreamingAssistantParserCheckpoint } from '../../../stores/streamingAssistantReveal';

  let root: HTMLElement;
  let value = $state({ text: 'seed' });
  const sink = createStreamingAssistantDomSink({
    getRoot: () => root,
    canAppendSource: () => true,
  });

  function checkpointFor(source: string): StreamingAssistantParserCheckpoint {
    let tailEnd = source.length;
    while (tailEnd > 0 && source.charCodeAt(tailEnd - 1) === 32) tailEnd--;
    return {
      tailSource: source,
      tailStart: 0,
      tailEnd,
      trailingAsciiSpaces: source.length - tailEnd,
    };
  }

  export function append(source: string, nextSource: string, delta: string): boolean {
    if (!sink.canAppendLiteral(source, checkpointFor(source), nextSource, delta)) return false;
    sink.appendLiteral(nextSource, delta);
    return true;
  }

  export async function resetAndRender(text: string): Promise<void> {
    sink.reset();
    // A fresh object invalidates Svelte's text effect even when the intended
    // string is unchanged. This is the transition that exposes an out-of-band
    // mutation of Svelte's private Text-node value cache.
    value = { text };
    await tick();
  }
</script>

<div bind:this={root}>
  <div class="md-volatile">
    <span data-streamdown-direct-append-safe style="display: contents">
      {value.text}
    </span>
  </div>
</div>
