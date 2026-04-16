<section class="rounded-2xl border border-border/70 bg-surface-1/75 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Archive</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Archived threads unavailable</h3>
      <p class="mt-1 max-w-2xl text-sm text-text-secondary">
        The frontend cannot render archived threads end-to-end because the current Wails API only exposes active-thread listing plus one-way archive.
      </p>
    </div>
    <span class="rounded-full border border-warning/35 bg-warning/12 px-3 py-1 text-[11px] font-medium text-warning">
      Backend blocked
    </span>
  </div>
</section>

<div class="rounded-2xl border border-warning/30 bg-warning/8 px-5 py-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]">
  <div class="flex items-start gap-3">
    <div class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-xl border border-warning/30 bg-warning/12 text-warning">
      <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M12 9v4" />
        <path d="M12 17h.01" />
        <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0Z" />
      </svg>
    </div>
    <div class="min-w-0">
      <p class="text-sm font-medium text-text-primary">What is missing</p>
      <ul class="mt-2 space-y-2 text-sm text-text-secondary" role="list">
        <li>
          `ListThreads()` filters out archived rows, so the settings view cannot enumerate archived conversations after a reload.
        </li>
        <li>
          `ArchiveThread()` only sets `archived = 1`; there is no `UnarchiveThread()` binding or toggle semantics to restore a thread.
        </li>
      </ul>
      <p class="mt-3 text-sm text-text-secondary">
        Archiving from the sidebar still works, but it is a one-way hide action in this build. Use delete only when you intend permanent removal.
      </p>
    </div>
  </div>
</div>
