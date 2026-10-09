// The app's one image lightbox. `App.svelte` mounts `ExpandedImageDialog`
// for whatever preview is open here, so any image can open it without a
// callback threaded through the surfaces between it and a host.
//
// A preview's `dispose` releases what its opener allocated for that
// lightbox lifetime. The store calls it exactly once: when the preview is
// replaced by another or closed. The new state is written before the old
// preview is disposed, so a throwing `dispose` cannot leave the store
// pointing at a released preview.

import type { ExpandedImagePreview } from '../utils/attachmentPreview.svelte';

// Raw: writers replace the preview wholesale, and its items carry functions
// the dialog calls; a deep proxy would buy nothing.
let current = $state.raw<ExpandedImagePreview | null>(null);

/** The open preview, or null. Reactive. */
export function imageLightbox(): ExpandedImagePreview | null {
  return current;
}

/** Show `preview`, disposing a different one that is still open. */
export function openImageLightbox(preview: ExpandedImagePreview): void {
  const previous = current;
  current = preview;
  if (previous !== preview) previous?.dispose?.();
}

/** Close the lightbox and dispose its preview. Closing twice is a no-op. */
export function closeImageLightbox(): void {
  const previous = current;
  current = null;
  previous?.dispose?.();
}
