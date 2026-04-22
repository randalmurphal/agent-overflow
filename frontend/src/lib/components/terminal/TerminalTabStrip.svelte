<script lang="ts">
  import type { ThreadTerminalStateHandle } from './terminalStore.svelte';

  interface Props {
    handle: ThreadTerminalStateHandle;
    onOpen: () => void;
    onClose: (terminalID: string) => void;
    onSelect: (terminalID: string) => void;
    onCollapse: () => void;
  }

  let { handle, onOpen, onClose, onSelect, onCollapse }: Props = $props();

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
          class:bg-green-500={running}
          class:bg-red-500={!running}
          aria-hidden="true"
        ></span>
        <span class="truncate max-w-[10ch]" title={`${tab.summary.shell} (pid ${tab.summary.pid})`}>
          {labelFor(tab.summary.shell)}
        </span>
        <button
          type="button"
          class="w-4 h-4 rounded hover:bg-surface-3 text-text-secondary hover:text-text-primary leading-none text-[11px]"
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
    aria-label="Open new terminal"
  >+</button>
  <button
    type="button"
    class="h-6 px-2 rounded hover:bg-surface-2 text-text-secondary hover:text-text-primary mr-1"
    data-testid="terminal-collapse"
    onclick={onCollapse}
    aria-label="Hide terminal drawer"
  >▾</button>
</div>
