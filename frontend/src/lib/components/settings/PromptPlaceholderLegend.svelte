<script lang="ts">
  // Reference for the placeholders the backend fills in at spawn, so a user
  // writing a prompt file knows what is available. Read-only: the prompt is
  // edited in its file. The same table, with the rules, is in the README the
  // backend writes beside the files.

  import { placeholdersFor } from '../../utils/promptOverrides';
  import type { ProviderID } from '../../providers/catalog';

  let { provider }: { provider: ProviderID } = $props();

  let placeholders = $derived(placeholdersFor(provider));

  function tokenName(token: string): string {
    return token.replace(/[{}]/g, '');
  }
</script>

<div
  class="rounded-[var(--radius-control)] border border-border-subtle/70 bg-surface-1/40 px-3.5 py-3"
  data-testid="settings-prompt-legend-{provider}"
>
  <div class="flex items-baseline gap-2">
    <span class="text-[0.65625rem] font-medium uppercase tracking-[0.16em] text-fg-hint">
      Placeholders
    </span>
    <span class="text-[0.71875rem] text-fg-muted">
      Filled in when a session starts. Any other braced text stays as written.
    </span>
  </div>

  <ul class="mt-2 grid gap-x-4 gap-y-1 sm:grid-cols-2">
    {#each placeholders as placeholder (placeholder.token)}
      <li class="flex items-baseline gap-2">
        <code
          class="shrink-0 select-all rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-1.5 py-0.5 font-mono text-[0.6875rem] text-fg-muted"
          data-testid="settings-prompt-token-{provider}-{tokenName(placeholder.token)}"
        >
          {placeholder.token}
        </code>
        <span class="text-[0.6875rem] leading-snug text-fg-muted">
          {placeholder.meaning}
        </span>
      </li>
    {/each}
  </ul>
</div>
