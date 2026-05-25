<script lang="ts">
  import AnsiText from './AnsiText.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import type { ClaudeSubagentTranscriptEntry } from '../../utils/claudeSubagentTranscript';

  let { entries }: { entries: ClaudeSubagentTranscriptEntry[] } = $props();

  function labelFor(entry: ClaudeSubagentTranscriptEntry): string {
    if (entry.kind === 'text') return entry.role === 'assistant' ? 'Assistant' : 'User';
    if (entry.kind === 'tool_use') return entry.toolName;
    return entry.toolName;
  }
</script>

<div class="space-y-2 px-3 py-2" data-testid="claude-subagent-transcript">
  {#each entries as entry}
    {@const classification = entry.kind === 'text' ? null : classifyToolName(entry.toolName)}
    <div
      class="rounded-[var(--radius-control)] border border-border-subtle/60 bg-surface-1/30 px-2 py-1.5"
      data-testid="claude-subagent-transcript-entry"
      data-entry-kind={entry.kind}
    >
      <div class="mb-1 flex items-center gap-1.5 text-[0.625rem] font-medium uppercase tracking-[0.04em] text-fg-hint">
        {#if classification}
          <ToolKindIcon kind={classification.icon} ariaLabel={classification.label} />
        {/if}
        <span>{labelFor(entry)}</span>
        {#if entry.kind === 'tool_result' && entry.isError}
          <span class="text-error normal-case tracking-normal">error</span>
        {/if}
      </div>
      {#if entry.kind === 'tool_use'}
        <pre class="whitespace-pre-wrap break-words font-mono text-[0.6875rem] leading-relaxed text-fg-muted">{entry.summary}</pre>
      {:else if entry.kind === 'tool_result'}
        <AnsiText
          source={entry.text}
          class="whitespace-pre-wrap break-words text-[0.6875rem] leading-relaxed {entry.isError ? 'text-error' : 'text-fg-muted'}"
        />
      {:else}
        <p class="whitespace-pre-wrap break-words text-[0.6875rem] leading-relaxed text-fg-muted">{entry.text}</p>
      {/if}
    </div>
  {/each}
</div>
