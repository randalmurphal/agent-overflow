<script lang="ts">
  import { slide } from 'svelte/transition';
  import DiffPreview from './DiffPreview.svelte';
  import type { DiffMeta, ChangedFile } from '../../types/models';

  interface DirGroup {
    dir: string;
    files: ChangedFile[];
  }

  let { files }: { files: ChangedFile[] } = $props();

  let expandedDirs = $state<Set<string>>(new Set());
  let expandedFile = $state<string | null>(null);

  let grouped = $derived.by((): DirGroup[] => {
    const dirs = new Map<string, ChangedFile[]>();
    for (const file of files) {
      const lastSlash = file.path.lastIndexOf('/');
      const dir = lastSlash === -1 ? '.' : file.path.slice(0, lastSlash);
      const existing = dirs.get(dir);
      if (existing) {
        existing.push(file);
      } else {
        dirs.set(dir, [file]);
      }
    }
    return [...dirs.entries()].map(([dir, dirFiles]) => ({ dir, files: dirFiles }));
  });

  function fileName(path: string): string {
    const lastSlash = path.lastIndexOf('/');
    return lastSlash === -1 ? path : path.slice(lastSlash + 1);
  }

  function toggleDir(dir: string) {
    const next = new Set(expandedDirs);
    if (next.has(dir)) {
      next.delete(dir);
    } else {
      next.add(dir);
    }
    expandedDirs = next;
  }

  function toggleFile(path: string) {
    expandedFile = expandedFile === path ? null : path;
  }

  function kindBadge(kind: DiffMeta['changeKind']): string {
    switch (kind) {
      case 'added': return 'bg-success/20 text-success';
      case 'modified': return 'bg-warning/20 text-warning';
      case 'deleted': return 'bg-error/20 text-error';
      case 'renamed': return 'bg-accent/30 text-accent';
    }
  }

  function diffMeta(file: ChangedFile): DiffMeta {
    return {
      filePath: file.path,
      changeKind: file.kind,
      insertions: file.insertions,
      deletions: file.deletions,
      preview: '',
    };
  }
</script>

<div class="mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/25 overflow-hidden">
  <div class="px-2.5 py-1.5 text-[11px] font-medium uppercase tracking-[0.06em] text-fg-subtle border-b border-border-subtle">
    {files.length} file{files.length !== 1 ? 's' : ''} changed
  </div>

  {#each grouped as group (group.dir)}
    <button
      class="w-full px-2.5 py-1 flex items-center gap-1.5 text-left cursor-pointer hover:bg-surface-2/25 border-b border-border-subtle/60 transition-colors"
      onclick={() => toggleDir(group.dir)}
      aria-expanded={expandedDirs.has(group.dir)}
      aria-label="Toggle directory: {group.dir}"
    >
      <span class="text-[11px] text-fg-subtle select-none" aria-hidden="true">{expandedDirs.has(group.dir) ? '▼' : '▶'}</span>
      <span class="text-[11px] font-mono text-fg-muted truncate">{group.dir}/</span>
      <span class="ml-auto text-[10px] text-fg-hint tabular-nums">{group.files.length}</span>
    </button>

    {#if expandedDirs.has(group.dir)}
      <div transition:slide={{ duration: 150 }}>
      {#each group.files as file (file.path)}
        <button
          class="w-full pl-7 pr-3 py-1 flex items-center gap-2 text-left cursor-pointer hover:bg-surface-2/30"
          onclick={() => toggleFile(file.path)}
          aria-expanded={expandedFile === file.path}
          aria-label="Toggle diff: {fileName(file.path)}, {file.kind}, +{file.insertions} -{file.deletions}"
        >
          <span class="text-xs font-mono text-text-primary truncate flex-1">{fileName(file.path)}</span>
          <span class="px-1.5 py-0.5 rounded-full text-[10px] {kindBadge(file.kind)}">{file.kind}</span>
          <span class="flex gap-1.5 text-[10px] tabular-nums shrink-0">
            {#if file.insertions > 0}
              <span class="text-success">+{file.insertions}</span>
            {/if}
            {#if file.deletions > 0}
              <span class="text-error">-{file.deletions}</span>
            {/if}
          </span>
        </button>

        {#if expandedFile === file.path}
          <div transition:slide={{ duration: 150 }} class="pl-7 pr-3 pb-2">
            <DiffPreview meta={diffMeta(file)} payloadId={file.payloadId} />
          </div>
        {/if}
      {/each}
      </div>
    {/if}
  {/each}
</div>
