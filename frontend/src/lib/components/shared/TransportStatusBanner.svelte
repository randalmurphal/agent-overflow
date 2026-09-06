<script lang="ts">
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import ComputerTransportStatus from './ComputerTransportStatus.svelte';
  const backend = $derived(selectedBackend());
</script>

{#key backend}
  <ComputerTransportStatus {backend} />
{/key}
