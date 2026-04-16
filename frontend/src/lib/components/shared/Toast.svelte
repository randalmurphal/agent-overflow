<script lang="ts">
  import { fly, fade } from 'svelte/transition';
  import { getToasts, removeToast, type ToastType } from '../../stores/toast.svelte';

  let toasts = $derived(getToasts());

  function iconPath(type: ToastType): string {
    switch (type) {
      case 'success':
        return 'M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0 1 12 2.944a11.955 11.955 0 0 1-8.618 3.04A12.02 12.02 0 0 0 3 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z';
      case 'error':
        return 'M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z';
      case 'warning':
        return 'M12 8v4m0 4h.01M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0z';
      case 'info':
        return 'M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0z';
    }
  }

  function colorClasses(type: ToastType): string {
    switch (type) {
      case 'success': return 'bg-success/15 border-success/30 text-success';
      case 'error':   return 'bg-error/15 border-error/30 text-error';
      case 'warning': return 'bg-warning/15 border-warning/30 text-warning';
      case 'info':    return 'bg-accent/15 border-accent/30 text-accent';
    }
  }
</script>

{#if toasts.length > 0}
  <div class="fixed bottom-4 right-4 z-50 flex flex-col gap-2 max-w-sm" aria-live="polite" aria-relevant="additions">
    {#each toasts as toast (toast.id)}
      <div
        in:fly={{ x: 80, duration: 200 }}
        out:fade={{ duration: 150 }}
        class="flex items-start gap-2.5 rounded-lg border px-3.5 py-2.5 shadow-lg backdrop-blur-sm text-sm {colorClasses(toast.type)}"
        role="alert"
      >
        <svg class="w-4 h-4 mt-0.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d={iconPath(toast.type)} />
        </svg>
        <span class="flex-1 leading-snug">{toast.message}</span>
        <button
          onclick={() => removeToast(toast.id)}
          class="shrink-0 opacity-60 hover:opacity-100 cursor-pointer rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          aria-label="Dismiss notification"
        >
          <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true">
            <path d="M18 6L6 18M6 6l12 12" />
          </svg>
        </button>
      </div>
    {/each}
  </div>
{/if}
