<script lang="ts">
  // Compact reference for the placeholders the backend fills in at spawn.
  // Clicking one inserts it at the caret of the prompt the user last touched.
  //
  // The buttons suppress mousedown's default so the click never moves focus:
  // the textarea keeps its caret and its selection, which is what the
  // insertion is relative to. Keyboard activation still works — an unfocused
  // textarea keeps its selection offsets, and the host refocuses it after
  // inserting.

  import { placeholdersFor } from '../../utils/promptOverrides';
  import type { ProviderID } from '../../providers/catalog';

  let {
    provider,
    onInsert,
  }: {
    provider: ProviderID;
    onInsert: (token: string) => void;
  } = $props();

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
      Click to insert. Any other braced text stays as written.
    </span>
  </div>

  <ul class="mt-2 grid gap-x-4 gap-y-1 sm:grid-cols-2">
    {#each placeholders as placeholder (placeholder.token)}
      <li class="flex items-baseline gap-2">
        <button
          type="button"
          class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-1.5 py-0.5 font-mono text-[0.6875rem] text-fg-muted transition-colors cursor-pointer hover:border-accent/40 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          data-testid="settings-prompt-token-{provider}-{tokenName(placeholder.token)}"
          title="Insert {placeholder.token}"
          onmousedown={(e) => e.preventDefault()}
          onclick={() => onInsert(placeholder.token)}
        >
          {placeholder.token}
        </button>
        <span class="text-[0.6875rem] leading-snug text-fg-muted">
          {placeholder.meaning}
        </span>
      </li>
    {/each}
  </ul>
</div>
