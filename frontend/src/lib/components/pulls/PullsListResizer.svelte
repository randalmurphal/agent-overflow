<script lang="ts">
  import { LIST_KEY_STEP_PX, type ListColumnWidth } from './listWidth.svelte';

  // The handle on the list column's trailing edge: drag it, or focus it
  // and use the arrow keys (Home and End for the narrowest and widest the
  // pane allows); a double click returns to the default. Both paths set
  // the width through the column's one clamp. The page renders it on
  // desktop only; compact has no column beside the detail.

  let { column }: { column: ListColumnWidth } = $props();

  function startDrag(event: PointerEvent): void {
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    const startX = event.clientX;
    const startWidth = column.width;
    handle.setPointerCapture(event.pointerId);
    const onMove = (move: PointerEvent) => {
      column.set(startWidth + (move.clientX - startX), false);
    };
    const onEnd = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onEnd);
      handle.removeEventListener('pointercancel', onEnd);
      column.remember();
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onEnd);
    handle.addEventListener('pointercancel', onEnd);
  }

  function onKeydown(event: KeyboardEvent): void {
    const next = (() => {
      switch (event.key) {
        case 'ArrowLeft':
          return column.width - LIST_KEY_STEP_PX;
        case 'ArrowRight':
          return column.width + LIST_KEY_STEP_PX;
        case 'PageUp':
          return column.width - LIST_KEY_STEP_PX * 4;
        case 'PageDown':
          return column.width + LIST_KEY_STEP_PX * 4;
        case 'Home':
          return column.bounds.min;
        case 'End':
          return column.bounds.max;
        default:
          return null;
      }
    })();
    if (next === null) return;
    event.preventDefault();
    column.set(next);
  }
</script>

<!-- A focusable separator is an interactive widget (WAI-ARIA window splitter). -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
<div
  role="separator"
  tabindex="0"
  aria-orientation="vertical"
  aria-label="Resize the list"
  aria-valuenow={column.width}
  aria-valuemin={column.bounds.min}
  aria-valuemax={column.bounds.max}
  class="absolute inset-y-0 -right-0.5 z-10 w-1 cursor-col-resize hover:bg-accent/40 focus:outline-none focus-visible:bg-accent/60 active:bg-accent/60"
  data-testid="pulls-list-resize"
  onpointerdown={startDrag}
  ondblclick={() => column.reset()}
  onkeydown={onKeydown}
></div>
