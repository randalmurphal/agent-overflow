// The review diff body's keyboard navigation: j/k step files, n/p step
// comment rows, c opens a file-level comment on the file at the top.
// Plain keys only, and never from a text field.

import type { LongListHandle } from '../virtual/LongListVirtualizer.svelte';
import type { ReviewFile } from '../../utils/patchStore';
import { REVIEW_FILE_HEADER_BAR_PX, type CommentAnchor, type ReviewRowsResult } from '../../utils/reviewRows';

export interface ReviewDiffKeysHost {
  built(): ReviewRowsResult;
  files(): readonly ReviewFile[];
  list(): LongListHandle | undefined;
  /** Scrolls to a row on the reader's behalf. */
  navigateToIndex(index: number, opts?: { offset?: number }): void;
  scrollTop(): number;
  onAddComment(): ((anchor: CommentAnchor) => void) | undefined;
}

/** The list row at the viewport top, or -1 with no rows. */
export function topRowIndex(host: ReviewDiffKeysHost): number {
  const list = host.list();
  if (!list || host.built().rows.length === 0) return -1;
  return list.findItemIndex(host.scrollTop());
}

function isInputTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName.toLowerCase();
  return tag === 'input' || tag === 'textarea' || tag === 'select' || target.isContentEditable;
}

export function reviewDiffKeydown(host: ReviewDiffKeysHost): (event: KeyboardEvent) => void {
  function topFileIndex(): number {
    const top = topRowIndex(host);
    return top >= 0 ? (host.built().fileOfRow[top] ?? -1) : -1;
  }

  function jumpFile(delta: 1 | -1): void {
    const fileIndex = topFileIndex();
    if (fileIndex < 0) return;
    const targetRow = host.built().firstRowOfFile[fileIndex + delta] ?? -1;
    if (targetRow >= 0) host.navigateToIndex(targetRow);
  }

  function jumpComment(delta: 1 | -1): void {
    const top = topRowIndex(host);
    if (top < 0) return;
    const rows = host.built().rows;
    for (let index = top + delta; index >= 0 && index < rows.length; index += delta) {
      const kind = rows[index]?.kind;
      if (kind === 'draft-editor' || kind === 'comment-thread' || kind === 'pr-thread') {
        host.navigateToIndex(index, { offset: -REVIEW_FILE_HEADER_BAR_PX });
        return;
      }
    }
  }

  function openFileLevelComment(): void {
    const add = host.onAddComment();
    if (!add) return;
    const fileIndex = topFileIndex();
    const file = fileIndex >= 0 ? host.files()[fileIndex] : null;
    if (file) add({ filePath: file.path, side: 'file', selectedText: '' });
  }

  const actions: Record<string, () => void> = {
    j: () => jumpFile(1),
    k: () => jumpFile(-1),
    n: () => jumpComment(1),
    p: () => jumpComment(-1),
    c: openFileLevelComment,
  };

  return (event) => {
    if (isInputTarget(event.target) || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    const action = actions[event.key];
    if (!action) return;
    event.preventDefault();
    action();
  };
}
