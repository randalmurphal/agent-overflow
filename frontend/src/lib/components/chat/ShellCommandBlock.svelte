<script lang="ts">
  // A command the person ran from the composer, as its user message shows it:
  // the command line, how it ended, and the first lines of its output. The
  // rest of the output is read when the block is opened (utils/shellOutput).
  // Whether it is open is the pane's row state, so a block that leaves the
  // timeline's window and returns is as the reader left it.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { formatAttachmentSize } from '../../types/attachment';
  import {
    formatShellDuration,
    shellDisplayText,
    shellOutcomeLabel,
    type ShellOutputMeta,
  } from '../../utils/shellInvocation';
  import { createShellOutputReader } from '../../utils/shellOutput.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import CopyButton from '../primitives/CopyButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import AnsiText from './AnsiText.svelte';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

  interface Props {
    /** The message row the block belongs to. */
    itemId: string;
    threadId: string;
    attachmentId: string;
    shell: ShellOutputMeta;
    /** Size of the output file in bytes. */
    size: number;
    pane?: ThreadPane;
    /** The block is the whole message, so it carries the bubble's shape. */
    standalone?: boolean;
  }

  let { itemId, threadId, attachmentId, shell, size, pane, standalone = false }: Props = $props();

  const reader = createShellOutputReader(() => threadId, () => attachmentId);
  const failed = $derived(shell.exitCode !== 0 || shell.timedOut === true);
  const outcome = $derived(shellOutcomeLabel(shell));
  const duration = $derived(formatShellDuration(shell.durationMs));
  const command = $derived(reader.command || shell.command);
  const commandCut = $derived(shell.commandLength !== undefined && !reader.command);
  const preview = $derived(shell.preview ?? '');
  const expandable = $derived(shell.more === true || shell.commandLength !== undefined);
  let localExpanded = $state(false);
  const expanded = $derived(
    expandable && (pane ? pane.isUserMessageExpanded(itemId, attachmentId) : localExpanded),
  );
  // Opening replaces the preview with the output read from its start, so the
  // text the reader was looking at stays where it was and grows downward.
  const body = $derived(shellDisplayText(expanded && reader.loaded ? reader.data : preview));

  // Reads the first page when the block is open, on a click and on a remount
  // that finds it open. A failed read waits for Retry.
  $effect(() => {
    if (expanded && !reader.loaded && !reader.loading && !reader.error) void reader.loadMore();
  });

  function toggle(event: MouseEvent): void {
    const next = !expanded;
    void preservePaneScrollAnchor(pane, event, () => {
      if (pane) pane.setUserMessageExpanded(itemId, next, attachmentId);
      else localExpanded = next;
    });
  }
</script>

<div
  class={[
    'group/shell min-w-0 overflow-hidden border border-border bg-surface-1 text-left',
    standalone ? 'rounded-[14px] rounded-br-[8px] shadow-sheet' : 'rounded-lg not-last:mb-2',
  ]}
  data-testid="shell-command-block"
  data-exit-code={shell.exitCode}
>
  <div class="flex items-start gap-2 px-3 py-2">
    <span class="select-none pt-px font-mono text-[0.75rem] leading-[1.5] text-accent" aria-hidden="true">$</span>
    <code
      class={[
        'min-w-0 flex-1 whitespace-pre-wrap break-words font-mono text-[0.75rem] leading-[1.5] text-fg',
        // Clamped only where the toggle below can open it.
        expandable && !expanded && 'line-clamp-3',
      ]}
      data-testid="shell-command-line"
    >{command}{#if commandCut}…{/if}</code>
    <span class="-my-1 shrink-0 opacity-0 transition-opacity duration-150 group-hover/shell:opacity-100 focus-within:opacity-100 compact:opacity-100">
      <CopyButton
        text={() => command}
        label="Copy command"
        onError={() => addToast('error', 'Failed to copy')}
      />
    </span>
    <span
      class={[
        'shrink-0 whitespace-nowrap pt-px text-[0.6875rem] leading-[1.6] tabular-nums',
        failed ? 'text-error' : 'text-fg-subtle',
      ]}
      data-testid="shell-command-outcome"
    >
      {outcome}{#if duration}<span class="text-fg-hint">{' · '}{duration}</span>{/if}
    </span>
  </div>

  {#if body !== '' || expanded}
    <div
      class={[
        'border-t border-border-subtle bg-surface-0/40 px-3 py-2',
        expanded && 'max-h-96 overflow-auto compact:max-h-[60vh]',
      ]}
      use:nestedScroll
      data-testid="shell-command-output"
    >
      {#if body !== ''}
        <AnsiText source={body} class="whitespace-pre-wrap break-words text-[0.6875rem] leading-relaxed text-fg-muted" />
      {:else if reader.loading}
        <p class="text-[0.6875rem] text-fg-subtle" role="status" aria-live="polite">Loading output…</p>
      {/if}
      {#if expanded && reader.error}
        <p class="mt-1 text-[0.6875rem] text-error" role="alert">
          Failed to load output: {reader.error}
          <button
            type="button"
            class="ml-1 cursor-pointer rounded text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            onclick={() => void reader.loadMore()}
          >Retry</button>
        </p>
      {/if}
      {#if expanded && reader.hasMore && !reader.error}
        <button
          type="button"
          class="mt-2 inline-flex items-center rounded-[var(--radius-control)] border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50"
          disabled={reader.loading}
          onclick={() => void reader.loadMore()}
          data-testid="shell-command-load-more"
        >
          {reader.loading ? 'Loading…' : `Load more (${formatAttachmentSize(reader.size)} total)`}
        </button>
      {/if}
    </div>
  {:else if size === 0}
    <div class="border-t border-border-subtle bg-surface-0/40 px-3 py-1.5 text-[0.6875rem] text-fg-hint" data-testid="shell-command-output">
      No output
    </div>
  {/if}

  {#if expandable || shell.truncated}
    <div class="flex items-center gap-2 border-t border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-subtle">
      {#if expandable}
        <button
          type="button"
          class="inline-flex cursor-pointer items-center gap-1 rounded px-1 py-0.5 hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none compact:py-1.5"
          aria-expanded={expanded}
          onclick={toggle}
          data-testid="shell-command-toggle"
        >
          <Icon icon={expanded ? ChevronDown : ChevronRight} size={11} strokeWidth={2.4} />
          {expanded ? 'Show less' : `Show all output (${formatAttachmentSize(size)})`}
        </button>
      {/if}
      {#if shell.truncated}
        <span class="ml-auto pr-1 text-fg-hint" title="The command wrote more than is kept. The start and the end are saved.">
          output cut in the middle
        </span>
      {/if}
    </div>
  {/if}
</div>
