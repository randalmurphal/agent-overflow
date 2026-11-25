// Files a person drags or pastes onto a surface: the composer's
// attachments and a ticket's. One reading of a drag's and a paste's
// payload, so both surfaces take the same files.

/**
 * True when the DragEvent is carrying file payloads (or an image MIME
 * type) we care about. Used to ignore noise from the many non-file
 * drag sources the browser fires (text, links, etc.).
 */
export function hasFilePayload(event: DragEvent): boolean {
  const types = event.dataTransfer?.types;
  if (!types) return false;
  return Array.from(types).some((type) => type === 'Files' || type.startsWith('image/'));
}

/**
 * The files a paste carries, in clipboard order; with `imagesOnly`, only
 * its images. Empty when it carries none, so its text stays the editor's.
 */
export function clipboardFiles(event: ClipboardEvent, imagesOnly = false): File[] {
  const clip = event.clipboardData;
  if (!clip) return [];
  const files: File[] = [];
  for (const item of Array.from(clip.items)) {
    if (item.kind !== 'file' || (imagesOnly && !item.type.startsWith('image/'))) continue;
    const file = item.getAsFile();
    if (file) files.push(file);
  }
  return files;
}
