<script lang="ts">
  import { onMount, tick } from 'svelte';
  import ChevronLeft from 'lucide-svelte/icons/chevron-left';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import { focusTrap } from '../../utils/focusTrap';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';

  interface Props {
    preview: ExpandedImagePreview;
    onClose: () => void;
  }

  let { preview, onClose }: Props = $props();
  let index = $state(0);
  let dialogRoot: HTMLDivElement | undefined = $state(undefined);
  let image = $derived(preview.images[index]);
  let hasMultiple = $derived(preview.images.length > 1);

  $effect(() => {
    index = preview.index;
  });

  function move(delta: number): void {
    if (!hasMultiple) return;
    index = (index + delta + preview.images.length) % preview.images.length;
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      onClose();
      event.stopPropagation();
    } else if (event.key === 'ArrowLeft') {
      event.preventDefault();
      move(-1);
      event.stopPropagation();
    } else if (event.key === 'ArrowRight') {
      event.preventDefault();
      move(1);
      event.stopPropagation();
    }
  }

  onMount(() => {
    void tick().then(() => dialogRoot?.focus());
  });
</script>

<div
  bind:this={dialogRoot}
  class="fixed inset-0 z-[90] flex items-center justify-center bg-black/88 p-4"
  role="dialog"
  aria-modal="true"
  aria-label={image?.filename ?? 'Image Preview'}
  tabindex="-1"
  onkeydown={handleKeydown}
>
  <button
    type="button"
    aria-label="Close Image Preview"
    class="absolute inset-0 cursor-default"
    onclick={onClose}
  ></button>
  {#if image}
    <button
      type="button"
      aria-label="Close Image Preview"
      class="absolute right-4 top-4 rounded-full bg-white/10 p-2 text-white transition hover:bg-white/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/70"
      onclick={(event) => {
        event.stopPropagation();
        onClose();
      }}
    >
      <Icon icon={X} size={18} strokeWidth={2.25} class="opacity-100" />
    </button>

    {#if hasMultiple}
      <button
        type="button"
        aria-label="Previous Image"
        class="absolute left-4 top-1/2 -translate-y-1/2 rounded-full bg-white/10 p-2 text-white transition hover:bg-white/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/70"
        onclick={(event) => {
          event.stopPropagation();
          move(-1);
        }}
      >
        <Icon icon={ChevronLeft} size={22} strokeWidth={2.25} class="opacity-100" />
      </button>
      <button
        type="button"
        aria-label="Next Image"
        class="absolute right-4 top-1/2 -translate-y-1/2 rounded-full bg-white/10 p-2 text-white transition hover:bg-white/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/70"
        onclick={(event) => {
          event.stopPropagation();
          move(1);
        }}
      >
        <Icon icon={ChevronRight} size={22} strokeWidth={2.25} class="opacity-100" />
      </button>
    {/if}

    <div
      use:focusTrap={{ active: true }}
      class="relative flex max-h-[92vh] max-w-[96vw] flex-col items-center gap-3"
      tabindex="-1"
    >
      <img
        src={image.url}
        alt={image.filename}
        class="max-h-[86vh] max-w-[92vw] object-contain"
      />
      <div class="max-w-[92vw] truncate text-xs text-white/78">
        {image.filename}{hasMultiple ? ` (${index + 1}/${preview.images.length})` : ''}
      </div>
    </div>
  {/if}
</div>
