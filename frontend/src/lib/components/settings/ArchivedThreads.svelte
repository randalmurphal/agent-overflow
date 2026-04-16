<section class="rounded-2xl border border-border/70 bg-surface-1/75 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
  <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Archive</p>
  <h3 class="mt-1 text-base font-semibold text-text-primary">Archived threads</h3>
  <p class="mt-1 text-sm text-text-secondary">This build does not expose the backend bindings needed to list or restore archived threads.</p>
</section>

<div class="rounded-2xl border border-amber-500/30 bg-amber-500/8 px-5 py-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]">
  <div class="flex items-start gap-3">
    <div class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-xl border border-amber-500/30 bg-amber-500/12 text-amber-300">
      <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M12 9v4" />
        <path d="M12 17h.01" />
        <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0Z" />
      </svg>
    </div>
    <div class="min-w-0">
      <p class="text-sm font-medium text-text-primary">Backend support is incomplete</p>
      <p class="mt-1 text-sm text-text-secondary">
        `ListThreads()` currently returns only active threads, and the available `ArchiveThread()` binding only archives in one direction. Until a dedicated archived-thread listing and unarchive binding exist, the frontend cannot render this section end-to-end.
      </p>
    </div>
  </div>
</div>
