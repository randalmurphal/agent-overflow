<script lang="ts">
  import { tick } from 'svelte';
  import type { EditorPreviewControl } from '../../stores/reviewEditorPreview.svelte';

  // The one Preview toggle of the review pane's editors, a quiet text
  // control in the editor's button row. On, it records where the text's
  // caret is and how tall its box stands (the review state keeps both
  // with the editor's text) and the host shows the rendered body in the
  // box's place; off, the text returns with the caret where it was, and
  // focus.

  interface Props {
    control: EditorPreviewControl;
    /** The editor's text box while it shows. */
    field: () => HTMLTextAreaElement | undefined;
    testid?: string;
  }

  let { control, field, testid = 'review-editor-preview-toggle' }: Props = $props();

  const on = $derived(control.state !== null);

  async function toggle(): Promise<void> {
    if (!on) {
      const box = field();
      control.open({ start: box?.selectionStart ?? 0, end: box?.selectionEnd ?? 0, height: box?.offsetHeight ?? 0 });
      return;
    }
    const was = control.close();
    await tick();
    const box = field();
    if (!box) return;
    box.focus({ preventScroll: true });
    if (was) box.setSelectionRange(was.start, was.end);
  }
</script>

<button
  type="button"
  class="h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-control)] px-2 text-[0.6875rem] {on ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:bg-surface-2 hover:text-fg'}"
  aria-pressed={on}
  title={on ? 'Back to the text' : 'Show how it will read'}
  data-editor-control="preview"
  data-testid={testid}
  onclick={() => { void toggle(); }}
>Preview</button>
