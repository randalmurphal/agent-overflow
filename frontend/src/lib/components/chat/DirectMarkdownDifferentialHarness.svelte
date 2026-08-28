<script lang="ts">
  import { tick } from 'svelte';
  import type { PathRef } from '../../types/models';
  import { StreamingAssistantRevealRouter } from '../../stores/streamingAssistantReveal';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import {
    createStreamingAssistantDomSink,
    sourceCompletesAllowlistedPath,
  } from './markdown/streamingAssistantDomSink';

  let {
    workspacePath = '',
    pathRefs = [],
  }: {
    workspacePath?: string;
    pathRefs?: PathRef[];
  } = $props();

  const ITEM_ID = 'direct-markdown-differential';
  const revealRouter = new StreamingAssistantRevealRouter();

  let canonical = '';
  let baselineSource = $state('');
  let directSource = $state('');
  let directRoot: HTMLElement;
  let directMounted = $state(true);
  const sink = createStreamingAssistantDomSink({
    getRoot: () => directRoot,
    canAppendSource: (source, nextSource) =>
      !sourceCompletesAllowlistedPath(pathRefs, source, nextSource),
  });

  $effect(() => {
    if (!directMounted) return;
    return revealRouter.register(ITEM_ID, sink);
  });

  export async function append(delta: string): Promise<boolean> {
    const previousSource = canonical;
    const previousCodeUnit = canonical.length > 0
      ? canonical.charCodeAt(canonical.length - 1)
      : -1;
    const nextSource = canonical + delta;
    baselineSource = nextSource;
    await tick();

    const appended = revealRouter.publish(
      ITEM_ID,
      previousCodeUnit,
      previousSource,
      delta,
      (next) => { canonical = next; },
    );
    if (!appended) {
      canonical = nextSource;
      directSource = nextSource;
      await tick();
    }
    return appended;
  }

  export async function synchronize(): Promise<void> {
    revealRouter.clearItem(ITEM_ID);
    directSource = canonical;
    await tick();
  }

  export async function remountDirect(): Promise<void> {
    revealRouter.clearItem(ITEM_ID);
    directMounted = false;
    await tick();
    directSource = canonical;
    directMounted = true;
    await tick();
  }
</script>

<div data-differential-baseline>
  <ChatMarkdown
    source={baselineSource}
    streaming
    {workspacePath}
    {pathRefs}
  />
</div>
<div bind:this={directRoot} data-differential-direct>
  {#if directMounted}
    <ChatMarkdown
      source={directSource}
      streaming
      {workspacePath}
      {pathRefs}
    />
  {/if}
</div>
