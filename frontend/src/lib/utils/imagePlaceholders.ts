import { imageAttachments, type Attachment } from '../types/attachment';

export const IMAGE_PLACEHOLDER_PREFIX = '[Image #';
export const IMAGE_PLACEHOLDER_SUFFIX = ']';

export interface PlaceholderInsertion {
  content: string;
  cursor: number;
}

export interface PlaceholderRange {
  attachmentId: string;
  attachmentIndex: number;
  label: string;
  start: number;
  end: number;
}

export interface PlaceholderRemoval {
  attachmentIds: string[];
  content: string;
  cursor: number;
}

export interface PlaceholderReconciliation {
  attachments: Attachment[];
  content: string;
  removedAttachmentIds: string[];
}

export function imagePlaceholderLabel(index: number): string {
  return `${IMAGE_PLACEHOLDER_PREFIX}${index}${IMAGE_PLACEHOLDER_SUFFIX}`;
}

export function insertImagePlaceholder(
  content: string,
  label: string,
  selectionStart: number,
  selectionEnd: number,
): PlaceholderInsertion {
  const start = clampOffset(selectionStart, content.length);
  const end = clampOffset(selectionEnd, content.length);
  const rangeStart = Math.min(start, end);
  const rangeEnd = Math.max(start, end);
  const before = content.slice(0, rangeStart);
  const after = content.slice(rangeEnd);
  const needsPrefixSpace = before.length > 0 && !/\s$/.test(before);
  const needsSuffixSpace = after.length > 0 && !/^\s/.test(after);
  const inserted = `${needsPrefixSpace ? ' ' : ''}${label}${needsSuffixSpace ? ' ' : ''}`;

  return {
    content: `${before}${inserted}${after}`,
    cursor: before.length + inserted.length,
  };
}

/**
 * Placeholders index IMAGES, not attachments: `[Image #2]` is the second
 * image in the draft no matter how many files sit between them. Every
 * numbering and matching pass in this module runs over `imageAttachments`
 * for that reason, and `attachmentIndex` is an index into THAT list.
 */
export function findImagePlaceholderRanges(content: string, attachments: Attachment[]): PlaceholderRange[] {
  const ranges: PlaceholderRange[] = [];
  const usedStarts = new Set<number>();

  imageAttachments(attachments).forEach((attachment, index) => {
    const label = imagePlaceholderLabel(index + 1);
    const start = findUnusedLabelStart(content, label, usedStarts);
    if (start === -1) return;

    usedStarts.add(start);
    ranges.push({
      attachmentId: attachment.id,
      attachmentIndex: index,
      label,
      start,
      end: start + label.length,
    });
  });

  return ranges.sort((a, b) => a.start - b.start);
}

export function removeImagePlaceholderByAttachmentId(
  content: string,
  attachments: Attachment[],
  attachmentId: string,
): PlaceholderRemoval | null {
  const ranges = findImagePlaceholderRanges(content, attachments);
  const range = ranges
    .find((placeholder) => placeholder.attachmentId === attachmentId);
  if (!range) return null;

  const nextAttachments = attachments.filter((attachment) => attachment.id !== attachmentId);
  const removed = removeRangeWithAdjacentWhitespace(content, range.start, range.end);
  const survivingRanges = adjustRangesAfterRemoval(
    ranges.filter((candidate) => candidate.attachmentId !== attachmentId),
    removed.start,
    removed.end,
  );
  return {
    attachmentIds: [attachmentId],
    content: renumberKnownImagePlaceholders(removed.content, survivingRanges, nextAttachments),
    cursor: removed.cursor,
  };
}

export function removeImagePlaceholderForKey(
  content: string,
  attachments: Attachment[],
  selectionStart: number,
  selectionEnd: number,
  key: 'Backspace' | 'Delete',
): PlaceholderRemoval | null {
  const ranges = findImagePlaceholderRanges(content, attachments);
  if (ranges.length === 0) return null;

  const start = clampOffset(selectionStart, content.length);
  const end = clampOffset(selectionEnd, content.length);
  const rangeStart = Math.min(start, end);
  const rangeEnd = Math.max(start, end);

  if (rangeStart !== rangeEnd) {
    return removeSelectedPlaceholders(content, attachments, ranges, rangeStart, rangeEnd);
  }

  const target = ranges.find((range) => {
    if (key === 'Backspace') {
      return rangeStart > range.start && rangeStart <= range.end;
    }
    return rangeStart >= range.start && rangeStart < range.end;
  });
  if (!target) return null;

  const nextAttachments = attachments.filter((attachment) => attachment.id !== target.attachmentId);
  const removed = removeRangeWithAdjacentWhitespace(content, target.start, target.end);
  const survivingRanges = adjustRangesAfterRemoval(
    ranges.filter((range) => range.attachmentId !== target.attachmentId),
    removed.start,
    removed.end,
  );
  return {
    attachmentIds: [target.attachmentId],
    content: renumberKnownImagePlaceholders(removed.content, survivingRanges, nextAttachments),
    cursor: removed.cursor,
  };
}

/**
 * Drop the images whose marker the user deleted by typing. A FILE is never
 * dropped here — it has no marker to lose, and its only removal gesture is
 * its own chip.
 */
export function reconcileImagePlaceholders(
  content: string,
  attachments: Attachment[],
): PlaceholderReconciliation {
  if (imageAttachments(attachments).length === 0) {
    return { content, attachments, removedAttachmentIds: [] };
  }

  const ranges = findImagePlaceholderRanges(content, attachments);
  const presentIds = new Set(ranges.map((range) => range.attachmentId));
  const survives = (attachment: Attachment) =>
    attachment.kind === 'file' || presentIds.has(attachment.id);
  const nextAttachments = attachments.filter(survives);
  if (nextAttachments.length === attachments.length) {
    return { content, attachments, removedAttachmentIds: [] };
  }

  return {
    attachments: nextAttachments,
    content: renumberKnownImagePlaceholders(content, ranges, nextAttachments),
    removedAttachmentIds: attachments
      .filter((attachment) => !survives(attachment))
      .map((attachment) => attachment.id),
  };
}

export function ensureImagePlaceholders(content: string, attachments: Attachment[]): string {
  let next = content;
  const presentIds = new Set(findImagePlaceholderRanges(content, attachments)
    .map((range) => range.attachmentId));
  imageAttachments(attachments).forEach((attachment, index) => {
    if (presentIds.has(attachment.id)) return;
    const label = imagePlaceholderLabel(index + 1);
    const insertion = insertImagePlaceholder(next, label, next.length, next.length);
    next = insertion.content;
  });
  return next;
}

function removeSelectedPlaceholders(
  content: string,
  attachments: Attachment[],
  ranges: PlaceholderRange[],
  selectionStart: number,
  selectionEnd: number,
): PlaceholderRemoval | null {
  const intersecting = ranges.filter((range) => rangesIntersect(
    selectionStart,
    selectionEnd,
    range.start,
    range.end,
  ));
  if (intersecting.length === 0) return null;

  const attachmentIds = intersecting.map((range) => range.attachmentId);
  const expandedStart = Math.min(selectionStart, ...intersecting.map((range) => range.start));
  const expandedEnd = Math.max(selectionEnd, ...intersecting.map((range) => range.end));
  const nextAttachments = attachments.filter((attachment) => !attachmentIds.includes(attachment.id));
  const contentWithoutSelection = content.slice(0, expandedStart) + content.slice(expandedEnd);
  const survivingRanges = adjustRangesAfterRemoval(
    ranges.filter((range) => !attachmentIds.includes(range.attachmentId)),
    expandedStart,
    expandedEnd,
  );

  return {
    attachmentIds,
    content: renumberKnownImagePlaceholders(contentWithoutSelection, survivingRanges, nextAttachments),
    cursor: expandedStart,
  };
}

function renumberKnownImagePlaceholders(
  content: string,
  ranges: PlaceholderRange[],
  nextAttachments: Attachment[],
): string {
  let next = content;
  const nextImages = imageAttachments(nextAttachments);
  const replacements = ranges
    .map((range) => {
      const nextIndex = nextImages.findIndex((attachment) => attachment.id === range.attachmentId);
      if (nextIndex === -1) return null;
      const nextLabel = imagePlaceholderLabel(nextIndex + 1);
      if (range.label === nextLabel) return null;
      return {
        start: range.start,
        end: range.end,
        label: nextLabel,
      };
    })
    .filter((replacement): replacement is { start: number; end: number; label: string } => replacement !== null)
    .sort((a, b) => b.start - a.start);

  for (const replacement of replacements) {
    next = next.slice(0, replacement.start) + replacement.label + next.slice(replacement.end);
  }

  return next;
}

function removeRangeWithAdjacentWhitespace(
  content: string,
  start: number,
  end: number,
): { content: string; cursor: number; start: number; end: number } {
  let removeStart = start;
  let removeEnd = end;
  const before = content[start - 1];
  const after = content[end];

  if (before && /\s/.test(before) && after && /\s/.test(after)) {
    removeStart -= 1;
  } else if (!before && after && /\s/.test(after)) {
    removeEnd += 1;
  } else if (before && /\s/.test(before) && !after) {
    removeStart -= 1;
  }

  return {
    content: content.slice(0, removeStart) + content.slice(removeEnd),
    cursor: removeStart,
    start: removeStart,
    end: removeEnd,
  };
}

function adjustRangesAfterRemoval(
  ranges: PlaceholderRange[],
  removedStart: number,
  removedEnd: number,
): PlaceholderRange[] {
  const removedLength = removedEnd - removedStart;
  return ranges.map((range) => {
    if (range.end <= removedStart) return range;
    return {
      ...range,
      start: range.start - removedLength,
      end: range.end - removedLength,
    };
  });
}

function findUnusedLabelStart(content: string, label: string, usedStarts: Set<number>): number {
  let start = content.indexOf(label);
  while (start !== -1) {
    if (!usedStarts.has(start)) return start;
    start = content.indexOf(label, start + label.length);
  }
  return -1;
}

function rangesIntersect(aStart: number, aEnd: number, bStart: number, bEnd: number): boolean {
  return aStart < bEnd && bStart < aEnd;
}

function clampOffset(offset: number, length: number): number {
  if (!Number.isFinite(offset)) return length;
  if (offset < 0) return 0;
  if (offset > length) return length;
  return offset;
}
