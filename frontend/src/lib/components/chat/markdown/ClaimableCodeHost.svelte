<script lang="ts">
  import StreamdownCodeHost from './StreamdownCodeHost.svelte';
  import { readCodeClaim } from './codeClaim';
  import type { ProvenAppend, Tokens } from '../../../markdown';

  // The code island of a ChatMarkdown with a host claim (codeClaim.ts):
  // the host's snippet for a block it claims, the ordinary code host
  // otherwise.

  let { token, id, textAppend }: { token: Tokens.Code; id: string; textAppend?: ProvenAppend } = $props();

  const read = readCodeClaim();
  const claim = $derived(read?.() ?? null);
</script>

{#if claim?.claims(token)}
  {@render claim.render(token)}
{:else}
  <StreamdownCodeHost {token} {id} {textAppend} />
{/if}
