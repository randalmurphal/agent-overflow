<script lang="ts">
  import { fly, fade } from 'svelte/transition';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import ErrorReportActions from './ErrorReportActions.svelte';
  import ErrorReportDetails from './ErrorReportDetails.svelte';
  import { holdToast, releaseToast, removeToast, getToasts } from '../../stores/toast.svelte';

  type ToastEntry = ReturnType<typeof getToasts>[number];
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

  let { toast, icon, colorClasses }: { toast: ToastEntry; icon: IconComponent; colorClasses: string } = $props();

  // An expanded toast stays until dismissed; otherwise it waits while the
  // pointer or focus is on it and resumes its delay after.
  let expanded = $state(false);
  let engaged = $state(false);

  function engage(): void {
    engaged = true;
    holdToast(toast.id);
  }

  function disengage(event: FocusEvent | PointerEvent): void {
    const card = event.currentTarget as HTMLElement;
    if (event instanceof FocusEvent && card.contains(event.relatedTarget as Node | null)) return;
    engaged = false;
    if (!expanded) releaseToast(toast.id);
  }

  function toggleDetails(): void {
    expanded = !expanded;
    if (expanded) holdToast(toast.id);
    else if (!engaged) releaseToast(toast.id);
  }
</script>

<div
  in:fly={{ x: 80, duration: 200 }}
  out:fade={{ duration: 150 }}
  class="rounded-[12px] border px-3.5 py-2.5 shadow-menu backdrop-blur-md text-[0.8125rem] transition-transform duration-150 {expanded ? '' : 'hover:scale-[1.015]'} {colorClasses}"
  role="alert"
  data-testid="toast"
  data-toast-type={toast.type}
  onpointerenter={engage}
  onpointerleave={disengage}
  onfocusin={engage}
  onfocusout={disengage}
>
  <div class="flex items-start gap-2.5">
    <span class="mt-0.5 shrink-0 flex items-center">
      <Icon {icon} size={14} strokeWidth={2} class="opacity-90" />
    </span>
    <div class="min-w-0 flex-1 leading-snug">
      <span class="break-words {expanded ? '' : 'line-clamp-3'}" data-testid="toast-message">{toast.message}</span>
      {#if toast.action}
        <Button size="xs" variant="ghost" class="mt-1" onclick={() => {
          removeToast(toast.id);
          toast.action?.run();
        }}>{toast.action.label}</Button>
      {/if}
    </div>
    <div class="shrink-0 flex items-center gap-0.5 -my-0.5">
      {#if toast.captured}
        <ErrorReportActions captured={toast.captured} {expanded} onToggle={toggleDetails} />
      {/if}
      <button
        onclick={() => removeToast(toast.id)}
        class="flex h-5 w-5 items-center justify-center opacity-60 hover:opacity-100 cursor-pointer rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-opacity"
        aria-label="Dismiss Notification"
      >
        <Icon icon={X} size={13} strokeWidth={2.5} class="opacity-100" />
      </button>
    </div>
  </div>
  {#if expanded && toast.captured}
    <ErrorReportDetails captured={toast.captured} />
  {/if}
</div>
