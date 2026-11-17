<script lang="ts">
  import Download from '@lucide/svelte/icons/download';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import SquareArrowOutUpRight from '@lucide/svelte/icons/square-arrow-out-up-right';
  import X from '@lucide/svelte/icons/x';
  import {
    browserDownloads,
    cancelBrowserDownload,
    clearBrowserDownloads,
    downloadProgressStep,
    downloadRunning,
    downloadSourceHost,
    downloadStatus,
    openBrowserDownload,
    revealBrowserDownload,
  } from '../../stores/browserDownloads.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Popover from '../primitives/Popover.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { browserPaneThread } from './browserPaneThread';

  // The address row's downloads button: there once the thread has a
  // download, showing how far the running ones are in steps, and opening
  // the list of the thread's downloads (internal/browser/downloads_thread.go).

  const thread = browserPaneThread();
  let threadId = $derived(thread());
  let downloads = $derived(threadId ? browserDownloads(threadId) : []);
  let running = $derived(downloads.filter(downloadRunning).length);
  let step = $derived(downloadProgressStep(downloads));
  let settled = $derived(downloads.length - running);

  let anchor: HTMLSpanElement | undefined = $state(undefined);
  let open = $state(false);

  let label = $derived(running > 0 ? `Downloads, ${running} running` : 'Downloads');

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: anchor?.querySelector<HTMLElement>('button') ?? undefined });
  }

  // The list goes away with its last row, so focus returns to the button.
  $effect(() => {
    if (open && downloads.length === 0) close();
  });
</script>

{#if threadId && downloads.length > 0}
  <span bind:this={anchor} class="relative inline-flex shrink-0" data-testid="browser-downloads-anchor">
    <IconButton
      {label}
      size="sm"
      testId="browser-downloads"
      ariaHaspopup="dialog"
      ariaExpanded={open}
      onClick={() => (open = !open)}
    >
      {#if running > 0 && step === null}
        <SteppedSpinner size={13} animate={!getSettings().lowPowerMode} />
      {:else}
        <Icon icon={Download} size={14} />
      {/if}
    </IconButton>
    {#if running > 0 && step !== null}
      <!-- Stepped: the bar moves only when the share crosses a step, and
           never animates between them. -->
      <span class="pointer-events-none absolute inset-x-1.5 bottom-0.5 h-0.5 rounded-full bg-surface-2" aria-hidden="true">
        <span class="block h-full rounded-full bg-accent" style:width="{step * 100}%" data-testid="browser-downloads-progress" data-step={step}></span>
      </span>
    {/if}
  </span>
  <Popover {anchor} {open} onClose={close} placement="bottom-end" role="dialog" ariaLabel="Downloads">
    <div class="flex w-80 max-w-[calc(100vw-1rem)] flex-col rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 py-1 shadow-menu" data-testid="browser-downloads-list">
      <ul class="flex max-h-80 flex-col overflow-y-auto">
        {#each downloads as download (download.id)}
          <li class="flex items-center gap-1.5 px-2.5 py-1.5" data-testid="browser-download" data-state={download.state}>
            <div class="min-w-0 flex-1">
              <div class="truncate text-xs text-fg" title={download.name}>{download.name}</div>
              <div
                class="truncate text-[0.6875rem] {download.state === 'failed' ? 'text-error' : 'text-fg-muted'}"
                title={download.source || undefined}
                data-testid="browser-download-status"
              >{downloadStatus(download)}{#if downloadSourceHost(download)}{` · ${downloadSourceHost(download)}`}{/if}</div>
            </div>
            {#if downloadRunning(download)}
              <IconButton label="Cancel download of {download.name}" title="Cancel" size="sm" testId="browser-download-cancel" onClick={() => void cancelBrowserDownload(threadId, download.id)}><Icon icon={X} size={14} /></IconButton>
            {:else if download.state === 'completed' && !download.error}
              <!-- A completed download with an error saved a file AO does
                   not open or show; its status says why. -->
              {#if download.openable}
                <IconButton label="Open {download.name} in a tab" title="Open in a tab" size="sm" testId="browser-download-open" onClick={() => { open = false; void openBrowserDownload(threadId, download.id); }}><Icon icon={SquareArrowOutUpRight} size={14} /></IconButton>
              {/if}
              <IconButton label="Show {download.name} in folder" title="Show in folder" size="sm" testId="browser-download-reveal" onClick={() => void revealBrowserDownload(threadId, download.id)}><Icon icon={FolderOpen} size={14} /></IconButton>
            {/if}
          </li>
        {/each}
      </ul>
      {#if settled > 0}
        <div class="flex justify-end border-t border-border-subtle px-2 pt-1">
          <Button variant="ghost" size="xs" testId="browser-downloads-clear" title="Remove finished downloads from this list; the files stay" onclick={() => void clearBrowserDownloads(threadId)}>Clear list</Button>
        </div>
      {/if}
    </div>
  </Popover>
{/if}
