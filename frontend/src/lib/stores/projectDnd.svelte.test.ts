import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  beginProjectDrag,
  computeReorderedIds,
  endProjectDrag,
  getDraggingProjectId,
  getDropPosition,
  getDropTargetProjectId,
  updateDropTarget,
} from './projectDnd.svelte';

// Minimal DragEvent stub. The store only uses dataTransfer.setData (in
// beginProjectDrag), preventDefault + dataTransfer.dropEffect (in
// updateDropTarget), and reads `clientY` against the row rect.
function makeDragEvent(opts: { clientY: number } = { clientY: 0 }): DragEvent {
  return {
    clientY: opts.clientY,
    dataTransfer: {
      setData: () => {},
      dropEffect: 'none',
      effectAllowed: 'none',
    } as unknown as DataTransfer,
    preventDefault: () => {},
  } as unknown as DragEvent;
}

function fakeRowEl(top: number, height: number): HTMLElement {
  return {
    getBoundingClientRect: () => ({
      top,
      bottom: top + height,
      left: 0,
      right: 100,
      width: 100,
      height,
      x: 0,
      y: top,
    }),
  } as unknown as HTMLElement;
}

describe('projectDnd store', () => {
  beforeEach(() => {
    endProjectDrag();
  });

  afterEach(() => {
    endProjectDrag();
  });

  describe('beginProjectDrag', () => {
    it('records the dragging id', () => {
      beginProjectDrag('p-a', makeDragEvent());
      expect(getDraggingProjectId()).toBe('p-a');
    });

    it('does nothing when the event has no dataTransfer', () => {
      const event = { clientY: 0 } as unknown as DragEvent;
      beginProjectDrag('p-a', event);
      expect(getDraggingProjectId()).toBeNull();
    });
  });

  describe('updateDropTarget', () => {
    it('does not register self as a drop target', () => {
      beginProjectDrag('p-a', makeDragEvent());
      updateDropTarget('p-a', makeDragEvent({ clientY: 5 }), fakeRowEl(0, 20));
      expect(getDropTargetProjectId()).toBeNull();
      expect(getDropPosition()).toBeNull();
    });

    it("classifies a cursor in the top half as 'before'", () => {
      beginProjectDrag('p-a', makeDragEvent());
      updateDropTarget('p-b', makeDragEvent({ clientY: 5 }), fakeRowEl(0, 20));
      expect(getDropTargetProjectId()).toBe('p-b');
      expect(getDropPosition()).toBe('before');
    });

    it("classifies a cursor in the bottom half as 'after'", () => {
      beginProjectDrag('p-a', makeDragEvent());
      updateDropTarget('p-b', makeDragEvent({ clientY: 15 }), fakeRowEl(0, 20));
      expect(getDropTargetProjectId()).toBe('p-b');
      expect(getDropPosition()).toBe('after');
    });

    it('is a no-op when no drag is in progress', () => {
      updateDropTarget('p-b', makeDragEvent({ clientY: 5 }), fakeRowEl(0, 20));
      expect(getDropTargetProjectId()).toBeNull();
    });
  });

  describe('computeReorderedIds', () => {
    function setupDrag(
      draggingId: string,
      targetId: string,
      position: 'before' | 'after',
    ): void {
      beginProjectDrag(draggingId, makeDragEvent());
      const clientY = position === 'before' ? 5 : 15;
      updateDropTarget(targetId, makeDragEvent({ clientY }), fakeRowEl(0, 20));
    }

    it('returns null when no drag is active', () => {
      expect(computeReorderedIds(['a', 'b', 'c'])).toBeNull();
    });

    it("returns null when dropping 'before' the immediately-following sibling (no-op)", () => {
      // Dragging A, dropping before B = stays at index 0.
      setupDrag('a', 'b', 'before');
      expect(computeReorderedIds(['a', 'b', 'c'])).toBeNull();
    });

    it("returns null when dropping 'after' the immediately-preceding sibling (no-op)", () => {
      setupDrag('b', 'a', 'after');
      expect(computeReorderedIds(['a', 'b', 'c'])).toBeNull();
    });

    it("places the dragged id 'before' a non-adjacent target", () => {
      setupDrag('c', 'a', 'before');
      expect(computeReorderedIds(['a', 'b', 'c'])).toEqual(['c', 'a', 'b']);
    });

    it("places the dragged id 'after' a non-adjacent target", () => {
      setupDrag('a', 'c', 'after');
      expect(computeReorderedIds(['a', 'b', 'c'])).toEqual(['b', 'c', 'a']);
    });

    it('returns null when the target id is not in currentOrder', () => {
      setupDrag('a', 'ghost', 'before');
      expect(computeReorderedIds(['a', 'b', 'c'])).toBeNull();
    });
  });

  describe('endProjectDrag', () => {
    it('clears all drag state', () => {
      beginProjectDrag('p-a', makeDragEvent());
      updateDropTarget('p-b', makeDragEvent({ clientY: 5 }), fakeRowEl(0, 20));
      endProjectDrag();
      expect(getDraggingProjectId()).toBeNull();
      expect(getDropTargetProjectId()).toBeNull();
      expect(getDropPosition()).toBeNull();
    });
  });
});
