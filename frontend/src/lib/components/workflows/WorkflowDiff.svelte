<script lang="ts">
  import { untrack } from 'svelte';
  import type { PatchFile } from '../../utils/patchFiles';
  interface Props {
    files: readonly PatchFile[];
    expandFirst?: boolean;
    onLoadFile?: (path: string) => Promise<PatchFile>;
  }
  let { files, expandFirst = false, onLoadFile }: Props = $props();
  let expanded = $state(new Set<string>());
  let loaded = $state(new Map<string, PatchFile>());
  let loading = $state(new Set<string>());
  let errors = $state(new Map<string, string>());
  $effect(() => {
    const first = files[0];
    const shouldExpand = expandFirst;
    if (!first) return;
    untrack(() => {
      if (shouldExpand) void expand(first);
      else collapse(first.path);
    });
  });

  function collapse(path: string): void {
    if (!expanded.has(path) && !loaded.has(path)) return;
    const next = new Set(expanded);
    next.delete(path);
    expanded = next;
    const nextLoaded = new Map(loaded);
    nextLoaded.delete(path);
    loaded = nextLoaded;
  }

  async function expand(file: PatchFile): Promise<void> {
    if (expanded.has(file.path) || loading.has(file.path)) return;
    const nextLoading = new Set(loading).add(file.path);
    loading = nextLoading;
    const nextErrors = new Map(errors);
    nextErrors.delete(file.path);
    errors = nextErrors;
    try {
      const fullFile = file.lines.length > 0 || !onLoadFile ? file : await onLoadFile(file.path);
      loaded = new Map(loaded).set(file.path, fullFile);
      expanded = new Set(expanded).add(file.path);
    } catch (error) {
      errors = new Map(errors).set(file.path, error instanceof Error ? error.message : String(error));
    } finally {
      const done = new Set(loading);
      done.delete(file.path);
      loading = done;
    }
  }

  function toggle(file: PatchFile): void {
    if (expanded.has(file.path)) collapse(file.path);
    else void expand(file);
  }
</script>

{#if files.length > 0}
  <section class="space-y-1.5" data-testid="wf-diff">
    <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Changes</h3>
    {#each files as file (file.path)}
      <div class="overflow-hidden rounded-md border border-border-subtle" data-testid="wf-diff-file">
        <button class="flex w-full items-center gap-2 px-2.5 py-2 text-left text-xs hover:bg-surface-2" onclick={() => toggle(file)} data-testid="wf-diff-file-toggle">
          <span class="text-fg-muted">{expanded.has(file.path) ? '▼' : '▶'}</span>
          <span class="min-w-0 flex-1 truncate font-mono">{file.path}</span>
          <span class="text-success">+{file.additions}</span><span class="text-error">−{file.deletions}</span>
        </button>
        {#if loading.has(file.path)}
          <p class="border-t border-border-subtle px-2.5 py-2 text-xs text-fg-muted">Loading hunks…</p>
        {:else if errors.has(file.path)}
          <p class="border-t border-error/30 px-2.5 py-2 text-xs text-error">{errors.get(file.path)}</p>
        {:else if expanded.has(file.path)}
          <pre class="max-h-72 overflow-auto border-t border-border-subtle bg-surface-0 p-2 text-[11px] leading-5" data-testid="wf-diff-hunks">{(loaded.get(file.path) ?? file).lines.map((line) => line.content).join('\n')}</pre>
        {/if}
      </div>
    {/each}
  </section>
{/if}
