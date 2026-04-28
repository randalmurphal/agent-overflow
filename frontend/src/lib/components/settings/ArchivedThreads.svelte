<script lang="ts">
  // Settings panel explaining the archive workflow now that
  // UnarchiveThread is wired end-to-end. The sidebar owns the actual
  // list rendering (toggle "Include archived" in the sidebar filter
  // to see them); this panel just documents what the toggle does.
  import SettingsHeader from './SettingsHeader.svelte';
</script>

<div class="flex flex-col gap-6">
  <section>
    <SettingsHeader
      eyebrow="Archive"
      title="Archived Threads"
      description="Archived threads are hidden from the default sidebar. Toggle Archived in the sidebar filter row to see them, then use each row's Unarchive action to bring one back."
    >
      {#snippet badge()}
        <span
          class="rounded-full border border-accent/35 bg-accent/10 px-2.5 py-0.5 text-[11px] font-medium text-accent"
        >
          Two-way
        </span>
      {/snippet}
    </SettingsHeader>
  </section>

  <section>
    <p class="text-[10.5px] font-medium uppercase tracking-[0.16em] text-fg-hint">
      Workflow
    </p>
    <ul class="mt-3 flex flex-col gap-2 text-[13px] leading-relaxed text-fg-muted" role="list">
      <li class="flex gap-2">
        <span aria-hidden="true" class="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-fg-hint"></span>
        <span>Archiving stops the active provider session and hides the thread from the default list.</span>
      </li>
      <li class="flex gap-2">
        <span aria-hidden="true" class="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-fg-hint"></span>
        <span>The Command Palette entry "Thread: Unarchive (restore)" runs on the selected archived thread.</span>
      </li>
      <li class="flex gap-2">
        <span aria-hidden="true" class="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-fg-hint"></span>
        <span>Deletion is still permanent. Use it only when the conversation is no longer needed.</span>
      </li>
    </ul>
  </section>
</div>
