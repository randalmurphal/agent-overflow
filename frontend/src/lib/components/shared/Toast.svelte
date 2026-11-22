<script lang="ts">
  import CheckCircle2 from '@lucide/svelte/icons/check-circle-2';
  import AlertTriangle from '@lucide/svelte/icons/alert-triangle';
  import Info from '@lucide/svelte/icons/info';
  import XCircle from '@lucide/svelte/icons/x-circle';
  import ToastItem from './ToastItem.svelte';
  import { getToasts, type ToastType } from '../../stores/toast.svelte';
  import { airspaceSurface } from '../../utils/paneAirspace.svelte';

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

<!-- The stack paints over whatever pane sits in the corner, and a native
     browser view there paints over the page, so the stack registers as an
     airspace layer: the view yields while a toast covers it. A layer, not an
     overlay: a toast takes no keyboard, so the pane's commands stay live. -->
{#if toasts.length > 0}
  <div use:airspaceSurface={{ overlay: false }} class="fixed bottom-4 compact:bottom-[max(1rem,env(safe-area-inset-bottom))] right-4 z-[80] flex flex-col gap-2 max-w-[min(24rem,calc(100vw-2rem))]" aria-live="polite" aria-relevant="additions">
    {#each toasts as toast (toast.id)}
      <ToastItem {toast} icon={iconForType(toast.type)} colorClasses={colorClasses(toast.type)} />
    {/each}
  </div>
{/if}
