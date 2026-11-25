<script lang="ts">
  // The one ticket finder a thread's Link ticket… or Change ticket… opens
  // (stores/threadTicketLinks.svelte.ts). Mounted once at the app root, so
  // the finder outlives the menu that opened it. The finder and the
  // Issues components it is built from load on the first open through one
  // stable dynamic import, so the startup chunk carries only this host.
  import { closeThreadTicketFinder, threadTicketFinding } from '../../stores/threadTicketLinks.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';

  type FinderModule = typeof import('../issues/ThreadTicketFinder.svelte');
  let finderModule: Promise<FinderModule> | null = null;
  function loadFinder(): Promise<FinderModule> {
    finderModule ??= import('../issues/ThreadTicketFinder.svelte').catch((err: unknown) => {
      // A failed load is tried again on the next open.
      finderModule = null;
      addErrorToast(`The ticket finder could not load: ${userFacingError(err)}`, err);
      closeThreadTicketFinder();
      throw err;
    });
    return finderModule;
  }

  const finding = $derived(threadTicketFinding());
</script>

{#if finding}
  {#await loadFinder() then { default: ThreadTicketFinder }}
    {#key finding}
      <ThreadTicketFinder {finding} />
    {/key}
  {:catch}
    <!-- Said in a toast by the load. -->
  {/await}
{/if}
