<script lang="ts">
  // Scrim + full-height card frame for the app's full-surface overlays
  // (settings, workflows). Both mount in App.svelte as SIBLINGS of
  // <PaneHost>, layered above it: the pane tree stays mounted underneath, so
  // opening and closing rebuild nothing.
  //
  // The `{#if open}` gate lives here rather than at the call site so a
  // LazyOverlay host can keep the component mounted and still play the exit
  // transition.
  //
  // Esc is NOT handled here. It is keybinding-driven (`settings.close`,
  // `workflows.escape`), which is what gives each surface the precedence it
  // needs against the other Esc-bound commands; a local handler would consume
  // the same press twice.

  import type { Snippet } from 'svelte';
  import { fade } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';

  interface Props {
    open: boolean;
    ariaLabel: string;
    /** Runs on a click that lands on the scrim itself, never on the card. */
    onScrimClick: () => void;
    scrimTestId?: string;
    testId?: string;
    children: Snippet;
  }

  let { open, ariaLabel, onScrimClick, scrimTestId, testId, children }: Props = $props();

  function handleScrimClick(event: MouseEvent): void {
    if (event.target === event.currentTarget) onScrimClick();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
  <div
    class="fixed inset-0 z-40 flex items-stretch justify-center bg-black/45 p-4 backdrop-blur-sm md:p-8"
    data-testid={scrimTestId}
    onclick={handleScrimClick}
    transition:fade={{ duration: 120 }}
  >
    <div
      use:focusTrap={{ active: open }}
      class="flex h-full w-full max-w-5xl flex-col overflow-hidden rounded-[var(--radius-card)] border border-border-subtle bg-surface-1 shadow-modal"
      role="dialog"
      aria-modal="true"
      aria-label={ariaLabel}
      data-testid={testId}
    >
      {@render children()}
    </div>
  </div>
{/if}
