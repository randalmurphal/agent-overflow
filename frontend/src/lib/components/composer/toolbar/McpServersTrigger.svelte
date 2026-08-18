<script lang="ts">
  // Composer-toolbar entry for MCP server selection. Renders the
  // trigger button (icon + "MCP" + active count) and hosts the
  // popup that lets the user toggle which provider-configured
  // servers are active. The count reflects the provider-native
  // listing: it counts non-disabled rows for the pane's MCP entity
  // (the workspace for Claude, the app for Codex).
  //
  // This is the surface that HOLDS the entity — it is mounted for as
  // long as the composer is, where the menu comes and goes — so the
  // attach lives here and McpServersMenu only reads.

  import Plug from '@lucide/svelte/icons/plug';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import {
    attachMcpServers,
    mcpTargetFor,
    peekMcpServers,
  } from '../../../stores/mcpServers.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import McpServersMenu from './McpServersMenu.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  function openMenu(): void {
    if (!pane.thread) return;
    open = true;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { paneId: pane.paneId, triggerEl });
  }

  function handleTrigger(): void {
    if (open) closeMenu();
    else openMenu();
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'mcp', {
      isOpen: () => open,
      open: openMenu,
      close: closeMenu,
    });
  });

  // Value-stable deriveds: the raw pane.thread reference is replaced
  // on every streaming event; deriving the strings first keeps the
  // target (and the attach effect below) quiet while a turn runs.
  // `pane.threadId` is null for a draft placeholder, which is exactly
  // the "no thread row, list from config" signal the store wants.
  let provider = $derived(pane.thread?.provider ?? '');
  let threadId = $derived(pane.threadId ?? '');
  let workspacePath = $derived(pane.thread?.workspacePath ?? '');
  let target = $derived(mcpTargetFor(provider, threadId, workspacePath));
  let enabledCount = $derived(
    target ? peekMcpServers(target.key).filter((r) => !r.disabled).length : 0,
  );

  // Hold the entity while this composer is mounted, so the badge shows the
  // enabled count without waiting for a menu open. The listing itself never
  // spawns a provider health-check — only an open menu permits that.
  //
  // The effect tracks the KEY and nothing else. `target` is a fresh object
  // whenever the pane's thread id moves, and that moves without changing the
  // entity — a thread switch inside one workspace is the same Claude entity —
  // so tracking it released and re-attached, dropping the shared listing to
  // refcount zero and re-listing for a change the entity never saw. The ctx
  // reads through to the live target for the same reason the key does not:
  // the listing RPC picks its variant from whichever thread is current when
  // it RUNS.
  let mcpKey = $derived(target?.key ?? null);
  $effect(() => {
    const key = mcpKey;
    if (key === null) return;
    const handle = attachMcpServers(key, {
      get provider() {
        return target?.provider ?? '';
      },
      get threadId() {
        return target?.threadId ?? '';
      },
      get workspacePath() {
        return target?.workspacePath ?? '';
      },
    });
    return () => handle.release();
  });
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label="MCP servers"
  data-testid="composer-mcp-trigger"
  data-enabled-count={enabledCount}
  title="MCP servers enabled for this thread"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[0.6875rem] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={Plug} size={13} strokeWidth={1.75} class="opacity-80" />
  <span data-composer-toolbar-label="collapsible">MCP</span>
  {#if enabledCount > 0}
    <span
      class="inline-flex min-w-[16px] items-center justify-center rounded-full bg-accent/20 px-1 text-[0.625rem] font-semibold leading-tight text-fg"
      data-testid="composer-mcp-count"
    >
      {enabledCount}
    </span>
  {/if}
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<McpServersMenu
  anchor={triggerEl}
  {open}
  {pane}
  onClose={closeMenu}
/>
