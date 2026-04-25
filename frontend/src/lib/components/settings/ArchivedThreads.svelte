<script lang="ts">
  // Settings panel explaining the archive workflow now that
  // UnarchiveThread is wired end-to-end. The sidebar owns the actual
  // list rendering (toggle "Include archived" in the sidebar filter
  // to see them); this panel just documents what the toggle does.
</script>

<section class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 p-5">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Archive</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Archived Threads</h3>
      <p class="mt-1 max-w-2xl text-sm text-text-secondary">
        Archived threads are hidden from the default sidebar. Toggle
        <span class="font-medium text-text-primary">Archived</span> in the sidebar filter row to see them, then use
        each row's Unarchive action to bring one back.
      </p>
    </div>
    <span class="rounded-full border border-accent/35 bg-accent/12 px-3 py-1 text-[11px] font-medium text-accent">
      Two-way
    </span>
  </div>
</section>

<div class="rounded-[var(--radius-control)] border border-border-subtle bg-card/20 px-5 py-5">
  <div class="flex items-start gap-3">
    <div class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-xl border border-border/60 bg-surface-0/70 text-text-secondary">
      <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M21 8v13H3V8" />
        <path d="M1 3h22v5H1z" />
      </svg>
    </div>
    <div class="min-w-0">
      <p class="text-sm font-medium text-text-primary">Workflow</p>
      <ul class="mt-2 space-y-2 text-sm text-text-secondary" role="list">
        <li>Archiving stops the active provider session and hides the thread from the default list.</li>
        <li>The Command Palette entry "Thread: Unarchive (restore)" runs on the selected archived thread.</li>
        <li>Deletion is still permanent. Use it only when the conversation is no longer needed.</li>
      </ul>
    </div>
  </div>
</div>
