<script lang="ts">
  import { copyToClipboard } from '../../utils/clipboard';
  import { addToast } from '../../stores/toast.svelte';

  let { text, label }: { text: string; label?: string } = $props();

  let copied = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function handleCopy() {
    try {
      const ok = await copyToClipboard(text);
      if (ok) {
        copied = true;
        clearTimeout(timer);
        timer = setTimeout(() => { copied = false; }, 2000);
      } else {
        addToast('error', 'Failed to copy to clipboard');
      }
    } catch (err) {
      console.error('Clipboard copy failed:', err);
      addToast('error', 'Failed to copy to clipboard');
    }
  }
</script>

<button
  onclick={handleCopy}
  class="inline-flex items-center gap-1 text-text-secondary hover:text-text-primary cursor-pointer p-1 rounded hover:bg-surface-2/50"
  title={label ?? 'Copy to clipboard'}
  aria-label={label ?? 'Copy to clipboard'}
>
  {#if copied}
    <svg class="w-3.5 h-3.5 text-green-400" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
      <path d="M20 6L9 17l-5-5" />
    </svg>
  {:else}
    <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
    </svg>
  {/if}
</button>
