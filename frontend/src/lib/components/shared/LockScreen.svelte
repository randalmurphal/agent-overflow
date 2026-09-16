<script lang="ts">
  // Shared cover; the caller owns authentication and background timing.
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import { focusTrap } from '../../utils/focusTrap';

  interface Props {
    /** Run the platform prompt again. */
    onUnlock: () => void;
    description?: string;
    busy?: boolean;
    error?: string;
    help?: string;
  }

  let {
    onUnlock,
    description = 'Use your fingerprint, face, or screen lock to open Agent Overflow.',
    busy = false,
    error = '',
    help = '',
  }: Props = $props();
</script>

<div
  use:focusTrap
  role="dialog"
  aria-modal="true"
  aria-label="Agent Overflow locked"
  tabindex="-1"
  data-testid="app-lock"
  class="fixed inset-0 z-[100] flex items-center justify-center bg-surface-0 p-6"
>
  <div class="flex w-full max-w-80 flex-col items-center gap-6 text-center">
    <div class="flex flex-col items-center gap-2">
      <MicroLabel as="p" class="text-fg-hint">Agent Overflow</MicroLabel>
      <h1 class="text-lg font-semibold text-text-primary">Locked</h1>
      <p class="text-sm text-text-secondary">
        {description}
      </p>
    </div>
    <Button variant="primary" size="md" class="w-full" loading={busy} disabled={busy} onclick={onUnlock}>Unlock</Button>
    {#if error}<p class="text-sm text-error" role="alert">{error}</p>{/if}
    {#if help}<p class="text-xs text-fg-muted">{help}</p>{/if}
  </div>
</div>
