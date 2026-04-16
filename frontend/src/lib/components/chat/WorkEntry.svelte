<script lang="ts">
  import type { WorkEntryData } from '../../types/models';

  let { entry }: { entry: WorkEntryData } = $props();

  let typeLabel = $derived.by(() => {
    const t = entry.type.toLowerCase();
    // Claude tools: Read, Write, Edit, Glob, Grep, NotebookEdit
    // Codex tools: file_read, file_change, file
    if (t === 'read' || t === 'write' || t === 'edit' || t === 'glob' || t === 'grep'
        || t === 'notebookedit' || t === 'file_read' || t === 'file_write'
        || t === 'file_edit' || t === 'file_change' || t === 'file') {
      return '[F]';
    }
    // Claude tools: Bash
    // Codex tools: command, command_execution
    if (t === 'bash' || t === 'command' || t === 'command_execution' || t === 'shell') {
      return '[C]';
    }
    return '[T]';
  });

  let preview = $derived.by(() => {
    if (entry.meta == null) return '';
    if (typeof entry.meta === 'string') return entry.meta;
    if (typeof entry.meta === 'object') {
      const m = entry.meta as Record<string, unknown>;
      // Common meta shapes from providers.
      if (typeof m.preview === 'string') return m.preview;
      if (typeof m.filePath === 'string') return m.filePath;
      if (typeof m.command === 'string') return m.command;
      if (typeof m.description === 'string') return m.description;
    }
    return '';
  });

  let isRunning = $derived(entry.status === 'running');
</script>

<div
  role="status"
  aria-live="polite"
  aria-label="{entry.name ?? entry.type}: {entry.status}"
  class="bg-surface-1 rounded border border-border px-3 py-2 text-sm
    {isRunning ? 'border-l-2 border-l-accent' : ''}"
>
  <div class="flex items-center gap-2">
    <span class="font-mono text-xs text-text-secondary shrink-0" aria-hidden="true">{typeLabel}</span>
    <span class="font-medium text-text-primary truncate">
      {entry.name ?? entry.type}
    </span>
    <span class="ml-auto shrink-0">
      {#if isRunning}
        <span class="text-accent animate-pulse">running</span>
      {:else}
        <span class="text-success">done</span>
      {/if}
    </span>
  </div>

  {#if preview}
    <p class="mt-1 text-xs text-text-secondary truncate">{preview}</p>
  {/if}
</div>
