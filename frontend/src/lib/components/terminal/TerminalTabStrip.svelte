<script lang="ts">
  import type { ThreadTerminalStateHandle } from './terminalStore.svelte';
  import EditorLink from '../common/EditorLink.svelte';

  interface Props {
    handle: ThreadTerminalStateHandle;
    onOpen: () => void;
    onClose: (terminalID: string) => void;
    onSelect: (terminalID: string) => void;
    /** Repaint the active terminal (PTY winsize nudge → provider redraw).
     *  Omitted when the host doesn't wire it; the ↻ button is hidden then,
     *  as it is whenever there are no tabs to refresh. */
    onRefresh?: () => void;
    /** Collapse the bottom drawer. Omitted in a full terminal pane, where
     *  there is nothing to collapse into — the ▾ button is hidden then. */
    onCollapse?: () => void;
    /** Workspace path for the terminal's owning thread. Optional —
     *  falls back to hiding the open-in-editor button when absent so
     *  detached / pre-thread states stay quiet. */
    workspacePath?: string;
  }

  let { handle, onOpen, onClose, onSelect, onRefresh, onCollapse, workspacePath }: Props =
    $props();

  function labelFor(shell: string): string {
    if (!shell) return 'shell';
    const parts = shell.split('/');
    return parts[parts.length - 1] || shell;
  }
</script>

<div class="flex items-center h-8 bg-surface-1 border-b border-border text-xs select-none shrink-0">
  <div class="flex items-center gap-1 overflow-x-auto flex-1 px-1">
    {#each handle.tabs as tab (tab.terminalID)}
      {@const isActive = tab.terminalID === handle.activeTerminalID}
      {@const running = tab.summary.running}
      <div
        class="flex items-center gap-1.5 pl-2 pr-1 h-6 rounded cursor-pointer border"
        class:bg-surface-2={isActive}
        class:border-accent={isActive}
        class:border-transparent={!isActive}
        class:text-text-primary={isActive}
        class:text-text-secondary={!isActive}
        data-testid={`terminal-tab-${tab.terminalID}`}
        onclick={() => onSelect(tab.terminalID)}
        onkeydown={(e) => {
          // WAI-ARIA tab pattern activates on Enter AND Space. Previous
          // version handled only Enter, so keyboard users couldn't
          // activate tabs with Space.
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            onSelect(tab.terminalID);
          }
        }}
        role="tab"
        aria-selected={isActive}
        tabindex="0"
      >
        <span
          class="w-1.5 h-1.5 rounded-full shrink-0"
          class:bg-success={running}
          class:bg-error={!running}
          aria-hidden="true"
        ></span>
        <span class="truncate max-w-[10ch]" title={`${tab.summary.shell} (pid ${tab.summary.pid})`}>
          {labelFor(tab.summary.shell)}
        </span>
        <button
          type="button"
          class="w-4 h-4 rounded hover:bg-surface-3 text-text-secondary hover:text-text-primary leading-none text-[0.6875rem]"
          data-testid={`terminal-tab-close-${tab.terminalID}`}
          onclick={(e) => { e.stopPropagation(); onClose(tab.terminalID); }}
          aria-label={`Close terminal ${labelFor(tab.summary.shell)}`}
        >x</button>
      </div>
    {/each}
  </div>

  <button
    type="button"
    class="h-6 px-2 rounded hover:bg-surface-2 text-text-secondary hover:text-text-primary mx-1"
    data-testid="terminal-open"
    onclick={onOpen}
    aria-label="Open New Terminal"
  >+</button>
  {#if onRefresh && handle.tabs.length > 0}
    <!-- Repaint the active terminal. Nudges the PTY winsize (rows blip +
         restore) so the provider's TUI reconciles a corrupted frame; the
         xterm grid is never resized, so the view shows only the provider's
         own redraw. Recovers the close→reopen glitch and any provider-side
         render desync without the user reaching for a manual window resize. -->
    <button
      type="button"
      class="h-6 px-2 rounded hover:bg-surface-2 text-text-secondary hover:text-text-primary mr-1"
      data-testid="terminal-refresh"
      onclick={() => onRefresh?.()}
      aria-label="Refresh Terminal"
      title="Refresh Terminal"
    >↻</button>
  {/if}
  {#if workspacePath}
    <!-- Open the terminal's working directory in the user's editor.
         The terminal is bound to `cwd: pane.thread.workspacePath`
         (see ThreadTerminalDrawer), so re-using that path keeps the
         affordance correct after the user `cd`s — we point at the
         original workspace root, not a derived current directory. -->
    <div class="mr-1" data-testid="terminal-open-in-editor">
      <EditorLink
        path={workspacePath}
        asIcon
        ariaLabel="Open Workspace in Editor"
        title="Open Workspace in Editor"
      />
    </div>
  {/if}
  {#if onCollapse}
    <button
      type="button"
      class="h-6 px-2 rounded hover:bg-surface-2 text-text-secondary hover:text-text-primary mr-1"
      data-testid="terminal-collapse"
      onclick={onCollapse}
      aria-label="Hide Terminal Drawer"
    >▾</button>
  {/if}
</div>
