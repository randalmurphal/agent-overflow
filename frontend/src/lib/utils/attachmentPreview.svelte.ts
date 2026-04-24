import { onDestroy, untrack } from 'svelte';
import { GetAttachmentData } from '../stores/bindings';

export interface AttachmentPreviewSource {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
}

export interface ImagePreviewItem {
  id: string;
  filename: string;
  mimeType: string;
  size: number;
  url: string;
}

export interface ExpandedImagePreview {
  images: ImagePreviewItem[];
  index: number;
}

export interface UserMessageMeta {
  attachments?: AttachmentPreviewSource[];
}

export function parseUserMessageAttachments(meta: string | undefined): AttachmentPreviewSource[] {
  if (!meta) return [];
  try {
    const parsed = JSON.parse(meta) as UserMessageMeta;
    return Array.isArray(parsed.attachments) ? parsed.attachments : [];
  } catch {
    return [];
  }
}

export async function loadAttachmentPreview(attachment: AttachmentPreviewSource): Promise<ImagePreviewItem> {
  const data = await GetAttachmentData(attachment.threadId, attachment.id);
  return {
    id: attachment.id,
    filename: attachment.filename,
    mimeType: attachment.mimeType,
    size: attachment.size,
    url: imagePreviewUrl(attachment.mimeType, data),
  };
}

function imagePreviewUrl(mimeType: string, base64Data: string): string {
  if (typeof URL.createObjectURL !== 'function') {
    return `data:${mimeType};base64,${base64Data}`;
  }
  const binary = atob(base64Data);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return URL.createObjectURL(new Blob([bytes], { type: mimeType }));
}

function revokePreview(preview: ImagePreviewItem | undefined): void {
  if (!preview?.url.startsWith('blob:')) return;
  URL.revokeObjectURL(preview.url);
}

export function buildExpandedImagePreview(images: ImagePreviewItem[], selectedId: string): ExpandedImagePreview | null {
  const selectedIndex = images.findIndex((image) => image.id === selectedId);
  if (selectedIndex < 0) return null;
  return { images, index: selectedIndex };
}

interface AttachmentPreviewOptions {
  shouldLoad?: () => boolean;
}

export function createAttachmentPreviews(
  getAttachments: () => AttachmentPreviewSource[],
  options: AttachmentPreviewOptions = {},
) {
  let previews = $state<Record<string, ImagePreviewItem>>({});
  const loading = new Map<string, Promise<ImagePreviewItem | null>>();
  let loadGeneration = 0;

  function loadPreview(attachment: AttachmentPreviewSource, generation: number): Promise<ImagePreviewItem | null> {
    const existing = untrack(() => previews[attachment.id]);
    if (existing) return Promise.resolve(existing);
    const pending = loading.get(attachment.id);
    if (pending) return pending;

    const load = loadAttachmentPreview(attachment)
      .then((preview) => {
        if (generation !== loadGeneration) {
          revokePreview(preview);
          return null;
        }
        previews = { ...previews, [attachment.id]: preview };
        return preview;
      })
      .catch((err) => {
        console.error('Failed to load attachment preview:', err);
        return null;
      })
      .finally(() => {
        loading.delete(attachment.id);
      });
    loading.set(attachment.id, load);
    return load;
  }

  $effect(() => {
    const attachments = getAttachments();
    const generation = ++loadGeneration;
    const nextIds = new Set(attachments.map((attachment) => attachment.id));
    const currentPreviews = untrack(() => previews);
    for (const [id, preview] of Object.entries(currentPreviews)) {
      if (!nextIds.has(id)) revokePreview(preview);
    }
    const retainedPreviews = Object.fromEntries(
      Object.entries(currentPreviews).filter(([id]) => nextIds.has(id)),
    );
    if (Object.keys(retainedPreviews).length !== Object.keys(currentPreviews).length) {
      previews = retainedPreviews;
    }

    if (options.shouldLoad && !options.shouldLoad()) return;

    for (const attachment of attachments) {
      void loadPreview(attachment, generation);
    }
  });

  onDestroy(() => {
    for (const preview of Object.values(previews)) {
      revokePreview(preview);
    }
  });

  return {
    previewFor(id: string): ImagePreviewItem | undefined {
      return previews[id];
    },

    expandedPreview(selectedId: string): ExpandedImagePreview | null {
      const orderedPreviews = getAttachments()
        .map((attachment) => previews[attachment.id])
        .filter((preview): preview is ImagePreviewItem => Boolean(preview));
      return buildExpandedImagePreview(orderedPreviews, selectedId);
    },

    async loadExpandedPreview(selectedId: string): Promise<ExpandedImagePreview | null> {
      const attachments = getAttachments();
      const generation = loadGeneration;
      await Promise.all(attachments.map((attachment) => loadPreview(attachment, generation)));
      const orderedPreviews = attachments
        .map((attachment) => previews[attachment.id])
        .filter((preview): preview is ImagePreviewItem => Boolean(preview));
      return buildExpandedImagePreview(orderedPreviews, selectedId);
    },
  };
}
