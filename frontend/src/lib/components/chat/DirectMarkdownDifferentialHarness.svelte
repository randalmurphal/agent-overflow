<script lang="ts">
  import { tick } from 'svelte';
  import type { PathRef } from '../../types/models';
  import { StreamingAssistantRevealRouter } from '../../stores/streamingAssistantReveal';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import {
    AllowlistedPathCompletionGuard,
    createStreamingAssistantLiteralOwner,
  } from './markdown/streamingAssistantLiteralOwner';

  let {
    workspacePath = '',
    pathRefs = [],
  }: {
    workspacePath?: string;
    pathRefs?: PathRef[];
  } = $props();

  const ITEM_ID = 'direct-markdown-differential';
  const RENDER_CONTEXT = {
    streaming: true,
    volatileTailVisible: true,
    pathLinksInert: false,
    workspacePath: '',
    previewKey: '',
  } as const;
  const revealRouter = new StreamingAssistantRevealRouter();

  let canonical = $state('');
  let baselineSource = $state('');
  let presentationRevision = $state(0);
  let directRoot: HTMLElement;
  let directMounted = $state(true);
  const directSource = $derived.by(() => {
    void presentationRevision;
    return revealRouter.parserSourceFor(ITEM_ID, canonical, RENDER_CONTEXT);
  });
  const pathCompletionGuard = new AllowlistedPathCompletionGuard();
  const sink = createStreamingAssistantLiteralOwner({
    getRoot: () => directRoot,
    canAppendSource: (source, nextSource, delta) =>
      !pathCompletionGuard.completes(pathRefs, source, nextSource, delta),
  });

  $effect(() => {
    if (!directMounted) return;
    return revealRouter.register(
      ITEM_ID,
      sink,
      () => { presentationRevision += 1; },
    );
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
    await tick();
    return appended;
  }

  export async function synchronize(): Promise<void> {
    revealRouter.clearItem(ITEM_ID);
    presentationRevision += 1;
    await tick();
  }

  /**
   * The completion shape: the wire replaces the streamed body with the
   * provider's final text, which may differ from what the reveal already
   * painted. `retireItem` retires the direct presentation and asks for the
   * authoritative render in the same task the canonical row is replaced in.
   */
  export async function complete(finalSource: string): Promise<void> {
    revealRouter.retireItem(ITEM_ID, () => { presentationRevision += 1; });
    canonical = finalSource;
    baselineSource = finalSource;
    await tick();
  }

  export async function remountDirect(): Promise<void> {
    directMounted = false;
    await tick();
    presentationRevision += 1;
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
