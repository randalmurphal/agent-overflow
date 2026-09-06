<script lang="ts">
  import ProviderAccountsSettings from './ProviderAccountsSettings.svelte';
</script>

<div class="flex flex-col gap-4">
  <section class="rounded-xl border border-border-subtle bg-surface-0 p-4">
    <ProviderAccountsSettings provider="claude" />
  </section>
  <section class="rounded-xl border border-border-subtle bg-surface-0 p-4">
    <ProviderAccountsSettings provider="codex" />
  </section>
</div>
