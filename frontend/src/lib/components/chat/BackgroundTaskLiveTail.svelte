<script lang="ts">
  // The live tail of a running background command's output, under the
  // command in its tray row's open body. Mounting attaches the launch's
  // watch (stores/backgroundTaskOutput.svelte.ts) and unmounting, which is
  // the row collapsing or its completion payload taking over, releases it.
  //
  // The tail follows its own bottom the way the digest clip and the
  // discussion do: the shared scroll controller owns every scrollTop write
  // and the reader's intent, so a wheel, key, touch or scrollbar gesture
  // away from the bottom pauses the follow, and returning to the bottom
  // resumes it. The box is a nested scroller inside the pane, so its
  // gestures are attributed to it and not read as the pane's reader
  // leaving the bottom. It never registers with the pane's controller slot.
  import { tick, untrack } from 'svelte';
  import AnsiText from './AnsiText.svelte';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import {
    attachBackgroundTaskOutput,
    backgroundTaskOutputKey,
    splitBackgroundTaskOutputKey,
    type BackgroundTaskOutputTail,
  } from '../../stores/backgroundTaskOutput.svelte';
  import type { EntityAttachment } from '../../stores/entityStore.svelte';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { createContentGeometryNotifier } from '../../utils/scroll/contentGeometryNotifier';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import { isLiveContentActive, LIVE_CONTENT_ACTIVE_HOLD_MS } from '../../utils/liveContentActivity';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';

  let { threadId, launchId }: { threadId: string; launchId: string } = $props();

  // The attach effect keys on the ids' VALUES: a parent re-rendering with
  // a fresh but equal pair must not release and re-acquire the watch.
  let launchKey = $derived(backgroundTaskOutputKey(threadId, launchId));
  let attachment = $state.raw<EntityAttachment<BackgroundTaskOutputTail> | null>(null);
  $effect(() => {
    const { threadId: thread, launchId: launch } = splitBackgroundTaskOutputKey(launchKey);
    const current = untrack(() => attachBackgroundTaskOutput(thread, launch));
    attachment = current;
    return () => {
      current.release();
      if (attachment === current) attachment = null;
    };
  });
  let tail = $derived(attachment?.current ?? null);
  let attachError = $derived(attachment?.error ?? null);
  let text = $derived(tail?.text ?? '');
  let error = $derived(tail?.error ?? attachError);
  let waiting = $derived(tail !== null && !tail.ended && text === '' && error === null);

  const scrollbarGeometry = createContentGeometryNotifier();
  const stick = createUseStickToBottomController({
    liveContentActive: () =>
      isLiveContentActive(performance.now(), tail?.lastAppendAt ?? 0, LIVE_CONTENT_ACTIVE_HOLD_MS),
    onContentGeometryProcessed: scrollbarGeometry.notify,
  });

  let scrollEl = $state<HTMLElement | undefined>();
  let contentEl = $state<HTMLDivElement | undefined>();
  let fadedTop = $state(false);

  $effect(() => {
    const scroll = scrollEl;
    const content = contentEl;
    if (!scroll || !content) return;
    untrack(() => {
      stick.attach(scroll, content);
      // A newly opened tail starts at its newest line; later growth then
      // follows through the same spring the timeline uses.
      stick.forceStick();
    });
    void tick().then(() => {
      if (scrollEl === scroll) stick.observe('content');
    });
    return () => stick.detach();
  });

  function handleScroll(): void {
    fadedTop = (scrollEl?.scrollTop ?? 0) > 1;
  }
</script>

<div
  class="relative"
  data-testid="background-task-live-tail"
  data-following={stick.isAtBottom ? 'true' : 'false'}
  data-ended={tail?.ended ? 'true' : undefined}
  data-truncated={tail?.truncated ? 'true' : undefined}
>
  {#if tail?.truncated}
    <p class="px-3 pt-1 text-[0.625rem] text-fg-subtle" data-testid="background-task-live-tail-truncated">
      Earlier output not shown{tail.size > 0 ? ` (${formatPayloadSize(tail.size)} so far)` : ''}
    </p>
  {/if}
  <!-- Ten lines of output tall: the box carries the output's type, so `lh`
       is the output's line height, and 1rem is its py-2. -->
  <div
    bind:this={scrollEl}
    class="pane-scroll-surface max-h-[min(40vh,calc(10lh+1rem))] overflow-y-auto overflow-x-hidden [overflow-anchor:none] px-3 py-2 text-[0.6875rem] leading-relaxed"
    use:nestedScroll
    onscroll={handleScroll}
    data-testid="background-task-live-tail-scroll"
    data-scroll-owner="controller"
    role="log"
    aria-live="off"
    aria-label="Live command output"
  >
    <div bind:this={contentEl}>
      {#if error}
        <p class="text-[0.6875rem] text-error" role="alert" data-testid="background-task-live-tail-error">
          Failed to follow output: {error}
        </p>
      {:else if waiting}
        <p class="text-[0.6875rem] text-fg-subtle" data-testid="background-task-live-tail-waiting">
          Waiting for output…
        </p>
      {:else if attachment === null || tail === null}
        <p class="text-[0.6875rem] text-fg-subtle" role="status" aria-live="polite">Loading output…</p>
      {:else}
        <AnsiText source={text} class="whitespace-pre-wrap break-words text-fg-muted" />
      {/if}
    </div>
  </div>
  <div
    aria-hidden="true"
    class="scroll-top-fade right-0"
    class:opacity-0={!fadedTop}
    style:--scroll-top-fade-depth="20px"
  ></div>
  <OverlayScrollbar
    target={scrollEl}
    contentGeometry={scrollbarGeometry}
    ariaLabel="Scroll live command output"
    placement="inset-y-0 right-0.5 w-1.5"
    ownerDrivenPosition={() => stick.positionOwnerDriven}
    onUserScrollStart={() => stick.markEscaped()}
    onUserScrollEnd={(atBottom) => {
      if (atBottom) stick.forceStick();
    }}
  />
</div>
