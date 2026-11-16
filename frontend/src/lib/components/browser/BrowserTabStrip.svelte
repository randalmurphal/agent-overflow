<script lang="ts">
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';
  import type { BrowserPageInfo } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';

  let {
    sessionName,
    pages,
    activePageId,
    onAct,
  }: {
    sessionName: string;
    pages: BrowserPageInfo[];
    activePageId: string;
    /** Runs a chrome action; an omitted pageId means the active page. */
    onAct: (kind: string, pageId?: string, address?: string, index?: number) => Promise<void>;
  } = $props();

  // ─── Tab drag-reorder ──────────────────────────────────────────────────
  // Same HTML5 DnD protocol the pane host uses for pane reordering. The
  // drop slot is a thin insertion bar between tabs; the drop commits a
  // "move" action and the authoritative order comes back with the state.

  const TAB_DRAG_MIME = 'application/x-ao-browser-tab';
  let dragPageId = $state('');
  let dropSlot = $state(-1);

  function tabDragStart(event: DragEvent, pageId: string): void {
    if (!event.dataTransfer) return;
    event.dataTransfer.setData(TAB_DRAG_MIME, pageId);
    event.dataTransfer.effectAllowed = 'move';
    dragPageId = pageId;
  }

  function tabDragOver(event: DragEvent, index: number): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    const tab = event.currentTarget as HTMLElement;
    const rect = tab.getBoundingClientRect();
    dropSlot = event.clientX < rect.left + rect.width / 2 ? index : index + 1;
  }

  function stripDragOver(event: DragEvent): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    // Only the strip's own empty tail; a tab under the pointer already set
    // its slot and stopped the event from bubbling this far unhandled.
    if (event.target !== event.currentTarget) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    dropSlot = pages.length;
  }

  function tabDrop(event: DragEvent): void {
    const pageId = event.dataTransfer?.getData(TAB_DRAG_MIME) ?? '';
    const slot = dropSlot;
    dragPageId = '';
    dropSlot = -1;
    if (!pageId || slot < 0) return;
    event.preventDefault();
    const from = pages.findIndex((page) => page.id === pageId);
    if (from < 0) return;
    // The backend's move index addresses the list WITHOUT the moved tab.
    const to = from < slot ? slot - 1 : slot;
    if (to === from) return;
    void onAct('move', pageId, '', to);
  }

  function tabDragEnd(): void {
    dragPageId = '';
    dropSlot = -1;
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="flex min-h-9 items-end gap-1 overflow-x-auto border-b border-border-subtle bg-surface-1 px-1 pt-1"
  onmousedown={(event) => {
    // Middle-press would start OS autoscroll; on a tab strip it closes tabs.
    if (event.button === 1) event.preventDefault();
  }}
  ondragover={stripDragOver}
  ondrop={tabDrop}
>
  {#if sessionName}
    <span class="mb-1 max-w-36 shrink-0 truncate px-1.5 text-[0.68rem] text-fg-muted" title={sessionName}>{sessionName}</span>
  {/if}
  {#each pages as page, tabIndex (page.id)}
    {#if dropSlot === tabIndex && dragPageId}
      <div class="h-6 w-0.5 shrink-0 self-center rounded bg-accent"></div>
    {/if}
    <div
      class="group flex h-8 min-w-28 max-w-48 items-center rounded-t-md {page.id === activePageId ? 'bg-surface-0 text-fg' : 'text-fg-muted hover:bg-surface-2'} {page.id === dragPageId ? 'opacity-50' : ''}"
      title={page.label ? `${page.label} — ${page.title || page.url}` : page.title || page.url}
      draggable="true"
      ondragstart={(event) => tabDragStart(event, page.id)}
      ondragover={(event) => tabDragOver(event, tabIndex)}
      ondrop={tabDrop}
      ondragend={tabDragEnd}
      onauxclick={(event) => {
        if (event.button !== 1) return;
        event.preventDefault();
        void onAct('close', page.id);
      }}
    >
      <button
        class="h-full min-w-0 flex-1 truncate pl-2 text-left text-[0.72rem]"
        onclick={() => void onAct('activate', page.id)}
      >{page.label || page.title || page.url || 'New tab'}</button>
      <button
        type="button"
        aria-label="Close tab"
        class="mr-1 rounded p-0.5 opacity-60 hover:bg-surface-3 hover:opacity-100"
        onclick={() => void onAct('close', page.id)}
      ><Icon icon={X} size={12} /></button>
    </div>
  {/each}
  {#if dropSlot === pages.length && dragPageId}
    <div class="h-6 w-0.5 shrink-0 self-center rounded bg-accent"></div>
  {/if}
  <button class="mb-1 rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="New tab" title="New tab" onclick={() => void onAct('new', '')}>
    <Icon icon={Plus} size={14} />
  </button>
</div>
