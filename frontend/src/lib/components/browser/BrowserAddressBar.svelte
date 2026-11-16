<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import SquareCode from '@lucide/svelte/icons/square-code';
  import X from '@lucide/svelte/icons/x';
  import type { BrowserPageInfo } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';

  let {
    activePage,
    sizeLabel,
    onAct,
    onRevealPageFile,
  }: {
    activePage: BrowserPageInfo | null;
    /** The page's layout size when it is not the pane's; empty hides it. */
    sizeLabel: string;
    /** Runs a chrome action; an omitted pageId means the active page. */
    onAct: (kind: string, pageId?: string, address?: string, index?: number) => Promise<void>;
    onRevealPageFile: () => Promise<void>;
  } = $props();

  let address = $state('');
  let addressFocused = $state(false);
  let activeIsLocalFile = $derived((activePage?.url ?? '').toLowerCase().startsWith('file:'));

  $effect(() => {
    const url = activePage?.url ?? '';
    if (!addressFocused) address = url;
  });
</script>

<div class="flex h-10 shrink-0 items-center gap-1 border-b border-border-subtle bg-surface-1 px-2">
  <button class="rounded p-1.5 text-fg-muted enabled:hover:bg-surface-2 enabled:hover:text-fg disabled:opacity-35" aria-label="Back" disabled={!activePage?.canGoBack} onclick={() => void onAct('back')}><Icon icon={ArrowLeft} size={14} /></button>
  <button class="rounded p-1.5 text-fg-muted enabled:hover:bg-surface-2 enabled:hover:text-fg disabled:opacity-35" aria-label="Forward" disabled={!activePage?.canGoForward} onclick={() => void onAct('forward')}><Icon icon={ArrowRight} size={14} /></button>
  <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Reload" onclick={() => void onAct('reload')}><Icon icon={RefreshCw} size={13} /></button>
  <input
    class="h-7 min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 text-[0.75rem] text-fg outline-none focus:border-accent/60"
    aria-label="Address"
    bind:value={address}
    disabled={!activePage}
    onfocus={() => (addressFocused = true)}
    onblur={() => (addressFocused = false)}
    onkeydown={(event) => {
      // Until the pane knows the page it shows there is none to name, and
      // a navigation naming none opens a new page.
      if (event.key === 'Enter' && activePage) {
        event.preventDefault();
        (event.currentTarget as HTMLInputElement).blur();
        void onAct('navigate', activePage.id, address);
      }
    }}
  />
  {#if sizeLabel}
    <span
      class="shrink-0 whitespace-nowrap px-1 font-mono text-[0.65rem] tabular-nums text-fg-muted"
      data-testid="browser-pane-size"
      title="The page is laid out at this size and shown scaled to fit the pane"
    >{sizeLabel}</span>
  {/if}
  {#if activeIsLocalFile}
    <button
      class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg"
      aria-label="Show in folder"
      title="Show in folder"
      onclick={() => void onRevealPageFile()}
    ><Icon icon={FolderOpen} size={13} /></button>
  {/if}
  <button
    class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg"
    aria-label="Open devtools"
    title="Open devtools"
    onclick={() => void onAct('devtools')}
  ><Icon icon={SquareCode} size={13} /></button>
  <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Close browser" title="Close browser" onclick={() => void onAct('hide', '')}><Icon icon={X} size={14} /></button>
</div>
