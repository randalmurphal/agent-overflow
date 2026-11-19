<script lang="ts">
  // What a settings page shows in place of its editors on a computer whose
  // release lacks its keys (computerHasSetting): nothing is written there.
  import { olderComputerNote } from '../../stores/settings.svelte';
  import type { BackendKey } from '../../transport/backendKey';

  let { backend, lacks }: { backend: BackendKey; lacks: string } = $props();
</script>

<p class="text-[0.8125rem] text-fg-muted" data-testid="settings-older-computer">{olderComputerNote(backend, lacks)}</p>
