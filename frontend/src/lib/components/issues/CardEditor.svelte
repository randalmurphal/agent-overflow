<script lang="ts" module>
  // The field editors load on the first change from a card or a row, once
  // per page.
  let fieldEditor: Promise<typeof import('./CardFieldEditor.svelte')> | null = null;
  function loadFieldEditor(): Promise<typeof import('./CardFieldEditor.svelte')> {
    fieldEditor ??= import('./CardFieldEditor.svelte');
    return fieldEditor;
  }
</script>

<script lang="ts">
  // The editor a Board card's or a Backlog row's mark opens
  // (`CardFieldEditor`), at `anchor`, loaded on first use; a load that
  // fails says so where the editor would be.
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import type { CardEditField } from './ticketEditors';

  interface Props {
    ctx: TicketWriteContext;
    row: RowView;
    field: CardEditField;
    unit: string;
    /** The tracker board an estimate is set on. */
    board: string;
    typed: boolean;
    anchor: HTMLElement;
    onClose: () => void;
  }

  let { ctx, row, field, unit, board, typed, anchor, onClose }: Props = $props();
</script>

{#await loadFieldEditor() then mod}
  <mod.default {ctx} {row} {field} {unit} {board} {typed} {anchor} {onClose} />
{:catch err}
  <p class="text-xs text-error" role="alert" data-testid="issues-card-editor-error">The editor could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
{/await}
