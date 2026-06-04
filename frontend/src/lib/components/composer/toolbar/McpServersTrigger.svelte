<script lang="ts">
  // Composer-toolbar entry for MCP server selection. Renders the
  // trigger button (icon + "MCP" + active count) and hosts the
  // popup that lets the user toggle which provider-configured
  // servers are active for this thread. The count reflects the
  // unified Disabled flag: it counts servers with !disabled for the
  // current thread's provider.

  import Plug from 'lucide-svelte/icons/plug';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import { mcpServersStore } from '../../../stores/mcpServers.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';
  import McpServersMenu from './McpServersMenu.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  function loadCurrentScope(): void {
    if (!pane.thread) return;
    if (pane.hasDraftPlaceholder) {
      void mcpServersStore.loadForNewThread(pane.thread.provider, pane.thread.workspacePath ?? '');
      return;
    }
    if (pane.threadId) {
      void mcpServersStore.loadForThread(pane.threadId, pane.thread.provider);
    }
  }

  function openMenu(): void {
    if (!pane.thread) return;
    open = true;
    // Prime the store on open. McpServersMenu also re-loads when it
    // mounts, but firing here avoids a flash of empty state.
    loadCurrentScope();
  }

  function closeMenu(): void {
    open = false;
    if (!focusPaneComposer(pane.paneId)) triggerEl?.focus();
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

  let provider = $derived(pane.thread?.provider ?? '');
  let threadId = $derived(pane.threadId ?? '');
  let workspacePath = $derived(pane.thread?.workspacePath ?? '');
  let isPlaceholder = $derived(pane.hasDraftPlaceholder);
  let enabledCount = $derived(
    provider
      ? (
        isPlaceholder
          ? mcpServersStore.serversForNewThread(provider, workspacePath)
          : threadId
            ? mcpServersStore.serversForThread(threadId, provider)
            : []
      ).filter((s) => !s.disabled).length
      : 0,
  );
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
