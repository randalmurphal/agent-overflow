<script lang="ts">
  import { tick } from 'svelte';
  import {
    attachStreamdownLiteralHost,
    type StreamdownLiteralHostHandle,
  } from '../../../markdown';
  import { createStreamingAssistantLiteralOwner } from './streamingAssistantLiteralOwner';
  import type { StreamingAssistantParserCheckpoint } from '../../../stores/streamingAssistantReveal';

  // The host renders EMPTY and its controller is the single writer, exactly as
  // the vendored `LiteralHost.svelte` mounts it. Driving the controller here
  // rather than mounting Streamdown keeps this a contract test for the owner.
  let root: HTMLElement;
  let hostElement: HTMLSpanElement;
  let handle: StreamdownLiteralHostHandle | undefined;
  let published = $state<{ token: object; text: string }>({ token: {}, text: 'seed' });
  const owner = createStreamingAssistantLiteralOwner({
    getRoot: () => root,
    canAppendSource: () => true,
  });

  $effect(() => {
    (handle ??= attachStreamdownLiteralHost(hostElement)).publish(
      published.token,
      published.text,
    );
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
    if (!owner.canAppendLiteral(source, checkpointFor(source), nextSource, delta)) {
      return false;
    }
    owner.appendLiteral(nextSource, delta);
    return true;
  }

  /**
   * The router's fallback: relinquish the run, then let an authoritative parser
   * update land. A fresh token object is what a re-lex produces, so this is the
   * transition that proves an unrelated-looking republish still reconciles.
   */
  export async function resetAndRender(text: string): Promise<void> {
    owner.reset();
    published = { token: {}, text };
    await tick();
  }
</script>

<div bind:this={root}>
  <div class="md-volatile">
    <span
      bind:this={hostElement}
      data-streamdown-direct-append-safe
      style="display: contents"
    ></span>
  </div>
</div>
