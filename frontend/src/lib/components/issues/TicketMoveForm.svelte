<script lang="ts">
  // The fields a move's screen takes, filled before the move is sent
  // (docs/specs/issues.md, "Ticket"). Each field's values are the move's
  // own (`TrackerChoices` with the move in its scope), a field of several
  // values takes each pick as one more, and a long text field is written
  // as Markdown from the text the ticket holds. Move sends the move once
  // with what the form holds; the form stays open until the move is made,
  // so a refusal, under the status, leaves what was filled. A required
  // field the app cannot set makes the form say so, with the way to the
  // tracker, and offers no Move.
  import { SvelteMap } from 'svelte/reactivity';
  import Button from '../primitives/Button.svelte';
  import TicketField from './TicketField.svelte';
  import type { PickItem } from './TicketPicker.svelte';
  import { fieldEditor, pickedOf, type EditorTarget } from './ticketEditors';
  import type { FieldEditInput, FieldView, MoveView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { fieldText } from '../../stores/trackerTicketModel';
  import { fieldChoices, moveTicket, ticketWrite, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';

  interface Props {
    ctx: TicketWriteContext;
    move: MoveView;
    /** The ticket's own fields: the values a move's field holds now. */
    fields: readonly FieldView[];
    fieldName: (id: string) => string;
    onOpenTracker: () => void;
    onClose: () => void;
  }

  let { ctx, move, fields, fieldName, onOpenTracker, onClose }: Props = $props();

  interface Filled {
    readonly edit: FieldEditInput;
    readonly picked: readonly PickItem[];
    readonly text: string;
  }
  const filled = new SvelteMap<string, Filled>();
  const pending = $derived(ticketWrite(ctx.key, 'status')?.pending ?? false);

  /** What the ticket holds in field `f` now, else the move's value. */
  function held(f: FieldView) {
    return fields.find((o) => o.id === f.id)?.value ?? f.value;
  }

  function targetOf(f: FieldView): EditorTarget {
    const got = filled.get(f.id);
    return {
      picked: got ? got.picked : pickedOf(held(f)),
      text: got ? got.text : fieldText(held(f)),
      choices: (text) => fieldChoices(ctx, f.id, text, move.id),
      save: (edit, picked) => filled.set(f.id, { edit, picked, text: edit.typed?.[0] ?? '' }),
    };
  }

  const rows = $derived((move.fields ?? []).map((f) => ({
    f,
    long: f.kind === 'rich_text' && f.editable,
    editor: f.kind === 'rich_text' ? null : fieldEditor(f, targetOf(f)),
  })));
  const unsettable = $derived(rows.filter((r) => r.f.required && !r.long && !r.editor).map((r) => r.f.name));

  function shown(f: FieldView): string {
    const got = filled.get(f.id);
    if (!got) return fieldText(held(f));
    return got.picked.length ? got.picked.map((p) => p.name).join(', ') : got.text;
  }

  function writeText(f: FieldView, markdown: string): void {
    filled.set(f.id, { edit: { field: f.id, markdown: { markdown, from: fieldText(held(f)) } }, picked: [], text: markdown });
  }

  async function submit(): Promise<void> {
    const edits = (move.fields ?? []).flatMap((f) => filled.get(f.id)?.edit ?? []);
    if ((await moveTicket(ctx, move.id, edits))?.kind === 'applied') onClose();
  }
</script>

<section class="flex flex-col gap-1.5 rounded-[var(--radius-field)] border border-border-subtle p-2.5" data-testid="ticket-move-form" data-move={move.id}>
  <p class="text-sm text-fg">Moving to {move.to.name || move.name} takes these fields:</p>
  {#each rows as { f, long, editor } (f.id)}
    {#if long}
      <label class="flex flex-col gap-1 py-1 text-xs text-fg-muted" data-testid="ticket-move-text-field" data-field={f.id}>
        {f.required ? `${f.name} (required)` : f.name}
        <textarea
          rows="4"
          class="w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-[0.8125rem] text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          value={shown(f)}
          disabled={pending}
          data-testid="ticket-move-text"
          oninput={(event) => writeText(f, event.currentTarget.value)}
        ></textarea>
      </label>
    {:else}
      <TicketField label={f.required ? `${f.name} (required)` : f.name} field={f.id} value={shown(f)} {editor} ticketKey={ctx.key} target="move-form" {fieldName} />
    {/if}
  {/each}
  {#if unsettable.length > 0}
    <div class="flex flex-wrap items-center gap-1.5 text-xs text-fg" role="status">
      <span data-testid="ticket-move-unsettable">Moving to {move.to.name || move.name} needs {unsettable.join(', ')}, which the app cannot set. Open the ticket in the tracker to make this move.</span>
      <Button size="sm" variant="ghost" testId="ticket-move-open-tracker" onclick={onOpenTracker}>Open in the tracker</Button>
    </div>
  {/if}
  <div class="flex items-center gap-1.5">
    {#if unsettable.length === 0}
      <Button size="sm" variant="primary" disabled={pending} testId="ticket-move-submit" onclick={() => { void submit(); }}>Move</Button>
    {/if}
    <Button size="sm" variant="ghost" disabled={pending} testId="ticket-move-cancel" onclick={onClose}>Cancel</Button>
  </div>
</section>
