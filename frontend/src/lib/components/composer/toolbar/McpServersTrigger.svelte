<script lang="ts">
  // Composer-toolbar entry for MCP server selection. Renders the
  // trigger button (icon + "MCP" + active count) and hosts the
  // popup that lets the user toggle which provider-configured
  // servers are active for this thread. The count reflects the
  // provider-native listing: it counts non-disabled rows for the
  // pane's scope.

  import Plug from 'lucide-svelte/icons/plug';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import { mcpServersStore, mcpScopeFor } from '../../../stores/mcpServers.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';
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
    // Prime the store on open. McpServersMenu's own load effect also
    // fires, but load() single-flights per scope, so this just starts
    // the background refresh a frame earlier while cached rows render.
    if (scope) void mcpServersStore.load(scope).catch(() => undefined);
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

  // Value-stable deriveds: the raw pane.thread reference is replaced
  // on every streaming event; deriving the strings first keeps the
  // scope (and the priming effect below) quiet while a turn runs.
  let provider = $derived(pane.thread?.provider ?? '');
  let threadId = $derived(pane.threadId ?? '');
  let workspacePath = $derived(pane.thread?.workspacePath ?? '');
  let isPlaceholder = $derived(pane.hasDraftPlaceholder);
  let scope = $derived(mcpScopeFor(provider, threadId, workspacePath, isPlaceholder));
  let enabledCount = $derived(
    scope ? mcpServersStore.rowsFor(scope).filter((r) => !r.disabled).length : 0,
  );

  // Prime the row cache when the pane's scope settles, so the badge
  // shows the enabled count without waiting for a menu open. noFetch:
  // priming must never spawn a provider health-check — the count comes
  // from config plus whatever the status cache already knows, and the
  // menu's own load (which may fetch) takes over on open. Failures
  // stay silent here; opening the menu surfaces the same error as a
  // toast.
  $effect(() => {
    if (!scope) return;
    void mcpServersStore.load(scope, { noFetch: true }).catch(() => undefined);
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
