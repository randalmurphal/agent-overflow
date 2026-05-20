const OVERFLOW_EPSILON_PX = 1;

export function measureComposerToolbarCompact(toolbar: HTMLElement): boolean {
  const previousCompact = toolbar.dataset.compact;
  const wasCompact = previousCompact === 'true';
  const availableWidth = toolbar.clientWidth;
  if (availableWidth <= 0) return wasCompact;

  // Force full-label mode for the read so a currently compact toolbar can
  // expand again as soon as the full content fits. Restore the attribute
  // afterward; Svelte remains the final owner of data-compact.
  if (wasCompact) toolbar.dataset.compact = 'false';
  const requiredWidth = toolbar.scrollWidth;
  const shouldCompact = requiredWidth > availableWidth + OVERFLOW_EPSILON_PX;

  if (wasCompact) toolbar.dataset.compact = previousCompact;
  return shouldCompact;
}
