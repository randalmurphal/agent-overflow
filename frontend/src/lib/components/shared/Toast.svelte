<script lang="ts">
  import { fly, fade } from 'svelte/transition';
  import CheckCircle2 from 'lucide-svelte/icons/check-circle-2';
  import AlertTriangle from 'lucide-svelte/icons/alert-triangle';
  import Info from 'lucide-svelte/icons/info';
  import XCircle from 'lucide-svelte/icons/x-circle';
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import { getToasts, removeToast, type ToastType } from '../../stores/toast.svelte';

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

  let toasts = $derived(getToasts());

  function iconForType(type: ToastType): IconComponent {
    switch (type) {
      case 'success': return CheckCircle2;
      case 'error':   return XCircle;
      case 'warning': return AlertTriangle;
      case 'info':    return Info;
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
  <div class="fixed bottom-4 right-4 z-[80] flex flex-col gap-2 max-w-sm" aria-live="polite" aria-relevant="additions">
    {#each toasts as toast (toast.id)}
      <div
        in:fly={{ x: 80, duration: 200 }}
        out:fade={{ duration: 150 }}
        class="flex items-start gap-2.5 rounded-[12px] border px-3.5 py-2.5 shadow-menu backdrop-blur-md text-[13px] transition-transform duration-150 hover:scale-[1.015] {colorClasses(toast.type)}"
        role="alert"
      >
        <span class="mt-0.5 shrink-0 flex items-center">
          <Icon icon={iconForType(toast.type)} size={14} strokeWidth={2} class="opacity-90" />
        </span>
        <span class="flex-1 leading-snug line-clamp-3">{toast.message}</span>
        <button
          onclick={() => removeToast(toast.id)}
          class="shrink-0 opacity-60 hover:opacity-100 cursor-pointer rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-opacity"
          aria-label="Dismiss notification"
        >
          <Icon icon={X} size={13} strokeWidth={2.5} class="opacity-100" />
        </button>
      </div>
    {/each}
  </div>
{/if}
