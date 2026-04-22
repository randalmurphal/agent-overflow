<!--
  Harness for Menu's async-hydrated-items case. Renders a loading row
  first; when `hydrated` flips to `true`, real MenuItems replace the
  placeholder. Mirrors how DiscussionsSubmenu and a cold-cache
  ProviderModelsSubmenu hydrate: the Menu mounts before any items
  exist, then items swap in after a binding round-trip.
-->
<script lang="ts">
  import Menu from '../Menu.svelte';
  import MenuItem from '../MenuItem.svelte';

  let {
    hydrated = false,
  }: {
    hydrated?: boolean;
  } = $props();
</script>

<Menu ariaLabel="Async menu">
  {#snippet children()}
    {#if !hydrated}
      <div class="px-3 py-2 text-xs" role="presentation" data-testid="async-menu-loading">
        Loading…
      </div>
    {:else}
      <MenuItem label="Alpha" />
      <MenuItem label="Bravo" />
      <MenuItem label="Charlie" />
    {/if}
  {/snippet}
</Menu>
