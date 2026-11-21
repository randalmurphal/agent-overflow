<script lang="ts" module>
  /** A tab's widest and narrowest width in px (browserTabStrip tests pin both). */
  export const TAB_MAX_WIDTH = 200;
  export const TAB_MIN_WIDTH = 72;
</script>

<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import CircleAlert from '@lucide/svelte/icons/circle-alert';
  import Globe from '@lucide/svelte/icons/globe';
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';
  import type { BrowserPageInfo } from '../../stores/bindings';
  import { browserAction, pageIdOf, type BrowserAction, type PageId } from '../../stores/browserActionWire';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import BrowserTabMenu from './BrowserTabMenu.svelte';

  // The browser's tab strip, drawn the way a desktop browser draws one:
  // tabs share the strip evenly between TAB_MAX_WIDTH and TAB_MIN_WIDTH, and
  // below that the tab row scrolls sideways (a vertical wheel scrolls it too)
  // with the active tab kept in view. The new-tab button sits right after
  // the last tab, outside the scroller, so it is never scrolled away. The
  // active tab shares the address row's surface and covers the strip's
  // bottom rule, so it reads as one piece with the row below.

  let {
    sessionName,
    pages,
    activePageId,
    closedTabs = 0,
    onAct,
    onNewTab,
    onEmptyDoubleClick,
  }: {
    sessionName: string;
    pages: BrowserPageInfo[];
    activePageId: string;
    /** How many closed tabs the thread can reopen. */
    closedTabs?: number;
    /** Runs a chrome action. */
    onAct: (action: BrowserAction) => Promise<void>;
    /** Opens a new tab the user asked for. */
    onNewTab: () => void;
    /** A double-click on the strip's empty space (maximize or restore the pane). */
    onEmptyDoubleClick?: () => void;
  } = $props();

  /** A tab's state, for its indicator and its test hook. */
  function tabState(page: BrowserPageInfo): string {
    if (page.error) return 'error';
    if (page.creating) return 'creating';
    if (page.restoring) return 'restoring';
    if (page.loading) return 'loading';
    if (page.suspended) return 'suspended';
    return 'ready';
  }

  function tabTitle(page: BrowserPageInfo, state: string): string {
    if (page.label || page.title) return page.label || page.title;
    if (state === 'creating') return 'Opening…';
    return page.url && page.url !== 'about:blank' ? page.url : 'New tab';
  }

  function tabTooltip(page: BrowserPageInfo, state: string): string {
    // A suspended tab with an error is a page that crashed (it reloads); any
    // other error is a tab that could not be opened.
    if (page.error) return page.suspended ? `This page stopped: ${page.error}` : `Could not open this tab: ${page.error}`;
    const title = tabTitle(page, state);
    const url = page.url && page.url !== 'about:blank' ? page.url : '';
    const name = page.label && page.title ? `${page.label} — ${page.title}` : title;
    const text = url && url !== name ? `${name}\n${url}` : name;
    return page.agent ? `${text}\nAn agent is using this page` : text;
  }

  // ─── The tab menu ───────────────────────────────────────────────────────
  // Held by page id, so a tab closed while the menu is open closes the menu
  // rather than pointing it at another tab.
  let menu: { x: number; y: number; pageId: string } | null = $state(null);
  let menuIndex = $derived(menu ? pages.findIndex((page) => page.id === menu?.pageId) : -1);

  function openMenu(event: MouseEvent, pageId: string): void {
    event.preventDefault();
    event.stopPropagation();
    menu = { x: event.clientX, y: event.clientY, pageId };
  }

  // ─── Keeping the active tab in view ────────────────────────────────────
  let scroller: HTMLDivElement | undefined = $state(undefined);

  // Scrolls the scroller alone: scrollIntoView would also scroll the pane
  // strip this pane sits in.
  function revealTab(pageId: string): void {
    const el = scroller?.querySelector<HTMLElement>(`[data-tab-id="${CSS.escape(pageId)}"]`);
    if (!scroller || !el) return;
    const box = el.getBoundingClientRect();
    const left = box.left - scroller.getBoundingClientRect().left + scroller.scrollLeft;
    const right = left + box.width;
    if (left < scroller.scrollLeft) scroller.scrollLeft = left;
    else if (right > scroller.scrollLeft + scroller.clientWidth) scroller.scrollLeft = right - scroller.clientWidth;
  }

  // The count and the active tab's place, not the list: every state push
  // brings a new list, and the measure forces a layout.
  let pageCount = $derived(pages.length);
  let activeIndex = $derived(pages.findIndex((page) => page.id === activePageId));
  $effect(() => {
    const id = activePageId;
    void pageCount;
    void activeIndex;
    if (id) revealTab(id);
  });

  // The strip's width changing (a pane resize, a promote) can push the
  // active tab out of view.
  $effect(() => {
    const el = scroller;
    if (!el) return;
    const observer = new ResizeObserver(() => {
      if (activePageId) revealTab(activePageId);
    });
    observer.observe(el);
    return () => observer.disconnect();
  });

  // A vertical wheel over an overflowing strip scrolls it sideways, as a
  // browser's does. Not passive: the page under the pane must not scroll.
  $effect(() => {
    const el = scroller;
    if (!el) return;
    const onWheel = (event: WheelEvent): void => {
      if (el.scrollWidth <= el.clientWidth || event.deltaX !== 0 || event.deltaY === 0) return;
      event.preventDefault();
      el.scrollLeft += event.deltaY;
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  });

  // ─── Tab drag-reorder ──────────────────────────────────────────────────
  // Same HTML5 DnD protocol the pane host uses for pane reordering. The
  // insertion indicator is drawn on the edge of the tab it lands beside, so
  // it moves nothing while the pointer moves; the drop commits a "move"
  // action and the authoritative order comes back with the state.

  const TAB_DRAG_MIME = 'application/x-ao-browser-tab';
  let dragPageId = $state('');
  // Where the drop lands, taken at drag-over from the tab the pointer is
  // over: that tab, and whether the drop is after it (its right half, or
  // the strip's empty space after the last tab). The drop names this tab
  // and side whatever the strip's order is by then, and the backend decides
  // at the move's turn where that puts the dragged tab, a no-op included.
  let dropAt = $state<{ pageId: string; after: boolean } | null>(null);

  function tabDragStart(event: DragEvent, pageId: string): void {
    if (!event.dataTransfer) return;
    event.dataTransfer.setData(TAB_DRAG_MIME, pageId);
    event.dataTransfer.effectAllowed = 'move';
    dragPageId = pageId;
  }

  function tabDragOver(event: DragEvent, pageId: string): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    const tab = event.currentTarget as HTMLElement;
    const rect = tab.getBoundingClientRect();
    const after = !(event.clientX < rect.left + rect.width / 2);
    if (dropAt?.pageId !== pageId || dropAt.after !== after) dropAt = { pageId, after };
  }

  function stripDragOver(event: DragEvent): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    // Only the strip's own empty space; a tab under the pointer already set
    // its slot and stopped the event from bubbling this far unhandled.
    if ((event.target as Element).closest('[data-tab-id]')) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    const last = pages.at(-1)?.id;
    if (last && (dropAt?.pageId !== last || !dropAt.after)) dropAt = { pageId: last, after: true };
  }

  function tabDrop(event: DragEvent): void {
    const pageId = event.dataTransfer?.getData(TAB_DRAG_MIME) ?? '';
    const at = dropAt;
    dragPageId = '';
    dropAt = null;
    if (!pageId || !at) return;
    event.preventDefault();
    // A tab dropped on itself is not moved.
    if (at.pageId === pageId) return;
    const beside = pageIdOf(at.pageId);
    if (beside) actOnPage(pageId, (id) => browserAction.move(id, beside, at.after));
  }

  // An action on the tab `raw` names; a blank id names none, and nothing is sent.
  function actOnPage(raw: string, build: (pageId: PageId) => BrowserAction): void {
    const pageId = pageIdOf(raw);
    if (pageId) void onAct(build(pageId));
  }

  function tabDragEnd(): void {
    dragPageId = '';
    dropAt = null;
  }

  function stripDoubleClick(event: MouseEvent): void {
    if ((event.target as Element).closest('[data-tab-id], button')) return;
    onEmptyDoubleClick?.();
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="relative flex h-9 shrink-0 items-end bg-surface-1 pl-1.5 pr-1 pt-1.5 compact:h-11"
  data-testid="browser-tab-strip"
  onmousedown={(event) => {
    // Middle-press would start OS autoscroll; on a tab strip it closes tabs.
    if (event.button === 1) event.preventDefault();
  }}
  ondblclick={stripDoubleClick}
  ondragover={stripDragOver}
  ondrop={tabDrop}
>
  <!-- The rule under the strip; the active tab paints over it. -->
  <div class="pointer-events-none absolute inset-x-0 bottom-0 h-px bg-border-subtle"></div>
  {#if sessionName}
    <span class="mb-2 mr-1 max-w-28 shrink-0 truncate px-1 text-[0.68rem] text-fg-muted" title={sessionName}>{sessionName}</span>
  {/if}
  <div
    bind:this={scroller}
    class="flex h-full min-w-0 flex-[0_1_auto] items-end overflow-x-auto overflow-y-hidden [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
    role="tablist"
    aria-label="Browser tabs"
    data-testid="browser-tab-scroller"
  >
    {#each pages as page, tabIndex (page.id)}
      {@const status = tabState(page)}
      {@const active = page.id === activePageId}
      {@const divided = !active && pages[tabIndex + 1]?.id !== activePageId && tabIndex < pages.length - 1}
      <div
        class={[
          // Every tab has the border, so activating one moves nothing.
          'group relative z-[1] flex h-full shrink items-center rounded-t-lg border border-b-0 @container',
          active
            ? 'border-border-subtle bg-surface-0 text-fg'
            : 'border-transparent text-fg-muted hover:text-fg',
          page.id === dragPageId ? 'opacity-50' : '',
        ].join(' ')}
        style:width="{TAB_MAX_WIDTH}px"
        style:min-width="{TAB_MIN_WIDTH}px"
        style:max-width="{TAB_MAX_WIDTH}px"
        data-tab-id={page.id}
        data-active={active ? 'true' : undefined}
        title={tabTooltip(page, status)}
        draggable="true"
        ondragstart={(event) => tabDragStart(event, page.id)}
        ondragover={(event) => tabDragOver(event, page.id)}
        ondrop={tabDrop}
        ondragend={tabDragEnd}
        onauxclick={(event) => {
          if (event.button !== 1) return;
          event.preventDefault();
          actOnPage(page.id, (id) => browserAction.onTab('close', id));
        }}
        oncontextmenu={(event) => openMenu(event, page.id)}
      >
        {#if !active}
          <!-- Hover fill, inset from the strip's edges like a browser's. -->
          <div class="pointer-events-none absolute inset-x-0.5 bottom-1 top-0.5 rounded-md group-hover:bg-surface-2/70"></div>
        {/if}
        {#if divided}
          <div class="pointer-events-none absolute right-0 top-1/2 h-4 w-px -translate-y-1/2 bg-border-subtle group-hover:opacity-0"></div>
        {/if}
        {#if dragPageId && dropAt?.pageId === page.id}
          <div class={['pointer-events-none absolute bottom-1 top-0.5 w-0.5 rounded bg-accent', dropAt.after ? '-right-px' : '-left-px']} data-testid="browser-tab-drop-indicator" data-side={dropAt.after ? 'after' : 'before'}></div>
        {/if}
        <button
          type="button"
          role="tab"
          aria-selected={active}
          class="relative flex h-full min-w-0 flex-1 items-center gap-2 pl-2.5 text-left text-[0.72rem] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40 rounded-t-lg"
          data-testid="browser-tab"
          data-tab-state={status}
          onclick={() => actOnPage(page.id, browserAction.select)}
        >
          <span
            class="flex size-4 shrink-0 items-center justify-center"
            data-testid="browser-tab-icon"
            data-icon={status === 'error' ? 'error' : status === 'creating' || status === 'restoring' || status === 'loading' ? 'spinner' : page.favicon ? 'favicon' : 'globe'}
          >
            {#if status === 'error'}
              <Icon icon={CircleAlert} size={14} class="text-error" />
            {:else if status === 'creating' || status === 'restoring' || status === 'loading'}
              <SteppedSpinner size={14} />
            {:else if page.favicon}
              <img src={page.favicon} alt="" class="size-4 object-contain" draggable="false" />
            {:else}
              <Icon icon={Globe} size={14} class="text-fg-muted" />
            {/if}
          </span>
          <span class="min-w-0 flex-1 truncate @max-[5.5rem]:hidden" data-testid="browser-tab-title">{tabTitle(page, status)}</span>
          {#if page.agent}
            <span class="flex shrink-0 animate-pulse text-accent" aria-label="An agent is using this page" data-testid="browser-tab-agent"><Icon icon={Bot} size={12} /></span>
          {/if}
        </button>
        <button
          type="button"
          aria-label="Close tab"
          title="Close tab{chordHintSuffix('pane.close')}"
          class={[
            'relative mr-1.5 flex size-5 shrink-0 items-center justify-center rounded-full text-fg-muted hover:bg-surface-3 hover:text-fg focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-8 compact:opacity-100 compact:pointer-events-auto',
            active ? '' : 'opacity-0 group-hover:opacity-100',
          ].join(' ')}
          data-testid="browser-tab-close"
          onclick={() => actOnPage(page.id, (id) => browserAction.onTab('close', id))}
        ><Icon icon={X} size={12} /></button>
      </div>
    {/each}
  </div>
  <button
    type="button"
    class="mb-1 ml-1 flex size-7 shrink-0 items-center justify-center self-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-9"
    aria-label="New tab"
    title="New tab{chordHintSuffix('browser.newTab')}"
    data-testid="browser-new-tab"
    onclick={() => onNewTab()}
  ><Icon icon={Plus} size={15} /></button>
  <!-- The strip's empty space: drop target for the end slot, double-click target. -->
  <div class="h-full min-w-2 flex-1" data-testid="browser-tab-strip-space"></div>
</div>

{#if menu && menuIndex >= 0}
  <BrowserTabMenu
    x={menu.x}
    y={menu.y}
    page={pages[menuIndex]}
    index={menuIndex}
    tabCount={pages.length}
    {closedTabs}
    {onAct}
    onDismiss={() => (menu = null)}
  />
{/if}
