<script lang="ts">
  import { highlightCode } from '../../utils/shiki';
  import CopyButton from './CopyButton.svelte';

  let { code, lang }: { code: string; lang: string } = $props();

  let html = $state('');
  let loading = $state(true);
  let highlightFailed = $state(false);

  $effect(() => {
    loading = true;
    highlightFailed = false;
    highlightCode(code, lang).then((result) => {
      html = result;
      loading = false;
    }).catch((err) => {
      console.error('Syntax highlighting failed:', err);
      highlightFailed = true;
      loading = false;
    });
  });
</script>

<div class="relative group rounded-md overflow-hidden border border-border bg-surface-0 mb-2">
  <div class="flex items-center justify-between px-3 py-1.5 bg-surface-1 border-b border-border text-xs text-text-secondary">
    <span>{lang}</span>
    <CopyButton text={code} label="Copy code" />
  </div>
  {#if loading || highlightFailed}
    <pre class="px-3 py-3 overflow-x-auto"><code class="text-xs font-mono text-text-secondary">{code}</code></pre>
  {:else}
    <div class="shiki-container px-3 py-3 overflow-x-auto text-xs [&_pre]:!bg-transparent [&_pre]:!p-0 [&_pre]:!m-0 [&_code]:!text-xs [&_code]:!font-mono">
      {@html html}
    </div>
  {/if}
</div>
