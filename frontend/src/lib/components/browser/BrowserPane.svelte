<script lang="ts">
  import { untrack } from 'svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import Plus from '@lucide/svelte/icons/plus';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import X from '@lucide/svelte/icons/x';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    attachBrowserCompanion,
    applyBrowserCompanionState,
    resizeBrowserCompanion,
  } from '../../stores/browserCompanion.svelte';
  import {
    BrowserCompanionAction,
    BrowserCompanionDo,
    BrowserCompanionInput,
    type BrowserCompanionInputEvent,
  } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';
  import { errString } from '../../utils/errors';

  let { ctx }: { ctx: PanelContext } = $props();
  let surface: HTMLDivElement | undefined = $state(undefined);
  let attachment = $state<ReturnType<typeof attachBrowserCompanion> | null>(null);
  let address = $state('');
  let addressFocused = $state(false);
  let error = $state('');
  let resizeTimer: ReturnType<typeof setTimeout> | null = null;
  let inputChain: Promise<void> = Promise.resolve();
  let pendingMove: BrowserCompanionInputEvent | null = null;
  let moveFrame = 0;
  let moveQueued = false;
  let pendingWheel: BrowserCompanionInputEvent | null = null;
  let wheelFrame = 0;
  let wheelQueued = false;

  let view = $derived(attachment?.current ?? null);
  let pages = $derived(view?.state.pages ?? []);
  let sessionName = $derived(view?.state.sessionName ?? '');
  let activePageId = $derived(view?.state.activePageId ?? '');
  let activePage = $derived(pages.find((page) => page.id === activePageId) ?? pages[0] ?? null);
  let frameSrc = $derived(view?.frame ? `data:image/jpeg;base64,${view.frame}` : '');

  $effect(() => {
    const threadId = ctx.threadId;
    if (!threadId) return;
    const rect = untrack(() => surface?.getBoundingClientRect());
    attachment = attachBrowserCompanion(threadId, Math.round(rect?.width ?? 900), Math.round(rect?.height ?? 700));
    return () => {
      if (moveFrame) cancelAnimationFrame(moveFrame);
      if (wheelFrame) cancelAnimationFrame(wheelFrame);
      moveFrame = 0;
      wheelFrame = 0;
      pendingMove = null;
      pendingWheel = null;
      attachment?.release();
      attachment = null;
    };
  });

  $effect(() => {
    const url = activePage?.url ?? '';
    if (!addressFocused) address = url;
  });

  $effect(() => {
    const el = surface;
    const threadId = ctx.threadId;
    if (!el || !threadId) return;
    const observer = new ResizeObserver((entries) => {
      const size = entries[0]?.contentRect;
      if (!size) return;
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        void resizeBrowserCompanion(threadId, Math.round(size.width), Math.round(size.height)).catch(() => {});
      }, 120);
    });
    observer.observe(el);
    return () => {
      observer.disconnect();
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = null;
    };
  });

  async function act(kind: string, pageId = activePageId, nextAddress = ''): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId) return;
    error = '';
    try {
      const state = await BrowserCompanionDo(threadId, new BrowserCompanionAction({
        kind,
        pageId,
        address: nextAddress,
      }));
      applyBrowserCompanionState(state);
    } catch (err) {
      error = errString(err);
    }
  }

  function queueInput(event: BrowserCompanionInputEvent): void {
    const threadId = ctx.threadId;
    const pageId = activePageId;
    if (!threadId || !pageId) return;
    inputChain = inputChain
      .then(() => BrowserCompanionInput(threadId, pageId, event))
      .catch((err) => { error = errString(err); });
  }

  // Pointer movement is lossy by nature. Keep at most one move queued behind
  // the in-flight input chain so a slow backend cannot turn hovering into an
  // unbounded promise/backlog; button/key edges stay ordered on the same chain.
  function queueLatestMove(): void {
    if (moveQueued || !pendingMove) return;
    const event = pendingMove;
    pendingMove = null;
    const threadId = ctx.threadId;
    const pageId = activePageId;
    if (!threadId || !pageId) return;
    moveQueued = true;
    inputChain = inputChain
      .then(() => BrowserCompanionInput(threadId, pageId, event))
      .catch((err) => { error = errString(err); })
      .finally(() => {
        moveQueued = false;
        if (pendingMove && !moveFrame) {
          moveFrame = requestAnimationFrame(() => {
            moveFrame = 0;
            queueLatestMove();
          });
        }
      });
  }

  function framePoint(event: PointerEvent | WheelEvent): { x: number; y: number } | null {
    const el = event.currentTarget as HTMLElement;
    const rect = el.getBoundingClientRect();
    const width = view?.frameWidth ?? 0;
    const height = view?.frameHeight ?? 0;
    if (!width || !height || !rect.width || !rect.height) return null;
    const scale = Math.min(rect.width / width, rect.height / height);
    const renderedWidth = width * scale;
    const renderedHeight = height * scale;
    const left = rect.left + (rect.width - renderedWidth) / 2;
    const top = rect.top + (rect.height - renderedHeight) / 2;
    const x = (event.clientX - left) / scale;
    const y = (event.clientY - top) / scale;
    if (x < 0 || y < 0 || x > width || y > height) return null;
    return { x, y };
  }

  function mouseButton(button: number): string {
    if (button === 1) return 'middle';
    if (button === 2) return 'right';
    return 'left';
  }

  function pointerInput(event: PointerEvent, kind: 'move' | 'down' | 'up'): void {
    const point = framePoint(event);
    if (!point) return;
    const input = {
      kind,
      ...point,
      button: mouseButton(event.button),
      buttons: event.buttons,
      clickCount: event.detail || 1,
    };
    if (kind === 'move') {
      pendingMove = input;
      if (!moveFrame) {
        moveFrame = requestAnimationFrame(() => {
          moveFrame = 0;
          queueLatestMove();
        });
      }
      return;
    }
    if (kind === 'down') {
      (event.currentTarget as HTMLElement).focus({ preventScroll: true });
      (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
    }
    queueInput(input);
  }

  function wheelInput(event: WheelEvent): void {
    const point = framePoint(event);
    if (!point) return;
    event.preventDefault();
    if (pendingWheel) {
      pendingWheel = {
        kind: 'wheel',
        ...point,
        deltaX: (pendingWheel.deltaX ?? 0) + event.deltaX,
        deltaY: (pendingWheel.deltaY ?? 0) + event.deltaY,
      };
    } else {
      pendingWheel = { kind: 'wheel', ...point, deltaX: event.deltaX, deltaY: event.deltaY };
    }
    if (!wheelFrame) {
      wheelFrame = requestAnimationFrame(() => {
        wheelFrame = 0;
        queueLatestWheel();
      });
    }
  }

  function queueLatestWheel(): void {
    if (wheelQueued || !pendingWheel) return;
    const input = pendingWheel;
    pendingWheel = null;
    const threadId = ctx.threadId;
    const pageId = activePageId;
    if (!threadId || !pageId) return;
    wheelQueued = true;
    inputChain = inputChain
      .then(() => BrowserCompanionInput(threadId, pageId, input))
      .catch((err) => { error = errString(err); })
      .finally(() => {
        wheelQueued = false;
        if (pendingWheel && !wheelFrame) {
          wheelFrame = requestAnimationFrame(() => {
            wheelFrame = 0;
            queueLatestWheel();
          });
        }
      });
  }

  function keyInput(event: KeyboardEvent): void {
    if (event.isComposing) return;
    if (event.key === 'Shift' || event.key === 'Control' || event.key === 'Alt' || event.key === 'Meta') return;
    event.preventDefault();
    event.stopPropagation();
    queueInput({
      kind: 'key',
      key: event.key,
      alt: event.altKey,
      control: event.ctrlKey,
      meta: event.metaKey,
      shift: event.shiftKey,
    });
  }
</script>

<div class="flex h-full min-h-0 flex-col bg-surface-0" data-testid="browser-pane">
  <div class="flex min-h-9 items-end gap-1 overflow-x-auto border-b border-border-subtle bg-surface-1 px-1 pt-1">
    {#if sessionName}
      <span class="mb-1 max-w-36 shrink-0 truncate px-1.5 text-[0.68rem] text-fg-muted" title={sessionName}>{sessionName}</span>
    {/if}
    {#each pages as page (page.id)}
      <div
        class="group flex h-8 min-w-28 max-w-48 items-center rounded-t-md {page.id === activePageId ? 'bg-surface-0 text-fg' : 'text-fg-muted hover:bg-surface-2'}"
        title={page.label ? `${page.label} — ${page.title || page.url}` : page.title || page.url}
      >
        <button
          class="h-full min-w-0 flex-1 truncate pl-2 text-left text-[0.72rem]"
          onclick={() => void act('activate', page.id)}
        >{page.label || page.title || page.url || 'New tab'}</button>
        <button
          type="button"
          aria-label="Close tab"
          class="mr-1 rounded p-0.5 opacity-60 hover:bg-surface-3 hover:opacity-100"
          onclick={() => void act('close', page.id)}
        ><Icon icon={X} size={12} /></button>
      </div>
    {/each}
    <button class="mb-1 rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="New tab" title="New tab" onclick={() => void act('new', '')}>
      <Icon icon={Plus} size={14} />
    </button>
  </div>

  <div class="flex h-10 shrink-0 items-center gap-1 border-b border-border-subtle bg-surface-1 px-2">
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Back" onclick={() => void act('back')}><Icon icon={ArrowLeft} size={14} /></button>
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Forward" onclick={() => void act('forward')}><Icon icon={ArrowRight} size={14} /></button>
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Reload" onclick={() => void act('reload')}><Icon icon={RefreshCw} size={13} /></button>
    <input
      class="h-7 min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 text-[0.75rem] text-fg outline-none focus:border-accent/60"
      aria-label="Address"
      bind:value={address}
      onfocus={() => (addressFocused = true)}
      onblur={() => (addressFocused = false)}
      onkeydown={(event) => {
        if (event.key === 'Enter') {
          event.preventDefault();
          (event.currentTarget as HTMLInputElement).blur();
          void act('navigate', activePageId, address);
        }
      }}
    />
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Close browser" title="Close browser" onclick={() => void act('hide', '')}><Icon icon={X} size={14} /></button>
  </div>

  {#if error}
    <div class="shrink-0 border-b border-error/25 bg-error/10 px-3 py-1.5 text-[0.7rem] text-error" role="alert">{error}</div>
  {/if}

  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    bind:this={surface}
    class="relative min-h-0 flex-1 overflow-hidden bg-surface-0 outline-none focus:ring-1 focus:ring-inset focus:ring-accent/70"
    role="application"
    tabindex="0"
    aria-label="Browser page"
    onpointerdown={(event) => pointerInput(event, 'down')}
    onpointermove={(event) => pointerInput(event, 'move')}
    onpointerup={(event) => pointerInput(event, 'up')}
    onpointercancel={(event) => pointerInput(event, 'up')}
    onwheel={wheelInput}
    onkeydown={keyInput}
    oncompositionend={(event) => queueInput({ kind: 'text', text: event.data })}
    oncontextmenu={(event) => event.preventDefault()}
  >
    {#if frameSrc}
      <img src={frameSrc} alt="" draggable="false" class="pointer-events-none h-full w-full select-none object-contain" />
    {:else if view?.error || attachment?.error}
      <div class="flex h-full items-center justify-center px-4 text-sm text-error">{view?.error || attachment?.error}</div>
    {:else}
      <div class="flex h-full items-center justify-center text-sm text-fg-muted">Connecting…</div>
    {/if}
  </div>
</div>
