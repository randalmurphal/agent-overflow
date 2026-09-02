<script lang="ts">
  // What the WebView shows while the platform's own gate has not passed
  // (docs/specs/remote-access.md § "Opening the app").
  //
  // It is deliberately almost nothing: a name, a line, and one button.
  // The prompt itself is the platform's, drawn over this by the OS, so
  // anything more here would be a second thing to look at behind a modal
  // nobody can dismiss to reach it. The button exists for the case after
  // the prompt is declined, where the screen would otherwise be a dead
  // end.
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';

  interface Props {
    /** Run the platform prompt again. */
    onUnlock: () => void;
  }

  let { onUnlock }: Props = $props();
</script>

<div class="flex min-h-screen items-center justify-center bg-surface-0 p-6">
  <div class="flex w-full max-w-80 flex-col items-center gap-6 text-center">
    <div class="flex flex-col items-center gap-2">
      <MicroLabel as="p" class="text-fg-hint">Agent Overflow</MicroLabel>
      <h1 class="text-lg font-semibold text-text-primary">Locked</h1>
      <p class="text-sm text-text-secondary">
        Unlock this phone's screen lock to open Agent Overflow.
      </p>
    </div>
    <Button variant="primary" size="md" class="w-full" onclick={onUnlock}>Unlock</Button>
  </div>
</div>
