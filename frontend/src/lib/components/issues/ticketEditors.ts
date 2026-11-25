// How a ticket field is changed in place (docs/specs/issues.md, "Ticket"):
// the one rule for which fields take an editor, what a pick or a typed
// value sends, and how a field of several values takes a pick. The ticket
// saves an edit at once (TicketScreen.svelte); a move's form collects
// them for the move (TicketMoveForm.svelte). Each passes its own target.

import type {
  ChoiceListView,
  FieldEditInput,
  FieldValueView,
  FieldView,
} from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import { fieldText } from '../../stores/trackerTicketModel';
import type { FieldEditor } from './TicketField.svelte';
import type { PickItem } from './TicketPicker.svelte';

/** Where an editor's values come from and where its edit goes. */
export interface EditorTarget {
  /** The values the field holds now. */
  readonly picked: readonly PickItem[];
  /** The value as typed text now. */
  readonly text: string;
  /** The values the field allows whose name starts with `text`. */
  choices(text: string): Promise<ChoiceListView>;
  /** Takes an edit, with the values a pick leaves the field holding. */
  save(edit: FieldEditInput, picked: readonly PickItem[]): void;
}

/** A field's values as the items a picker shows. */
export function pickedOf(value: FieldValueView): readonly PickItem[] {
  return (value.choices ?? []).map((c) => ({ id: c.id, name: c.name || c.id }));
}

/** The value a pick sends: the items, or empty to clear. */
function pickedValue(kind: string, items: readonly { id: string }[]): FieldValueView {
  return items.length === 0 ? {} as FieldValueView : { kind, choices: items.map((i) => ({ id: i.id })) } as FieldValueView;
}

/** A pick list for `field` of `kind` (option or user). A field of
 * several values takes a pick as one more value, or one fewer when it
 * holds it. */
export function pickEditor(field: string, kind: string, target: EditorTarget, opts: { multi?: boolean; clear?: string } = {}): FieldEditor {
  return {
    kind: 'pick',
    clearLabel: opts.clear,
    multi: opts.multi,
    checked: new Set(target.picked.map((p) => p.id)),
    load: async (text) => {
      const got = await target.choices(text);
      return { items: (got.choices ?? []).map((c) => ({ id: c.id, name: c.name || c.id })), more: got.more ?? false };
    },
    onPick: (item) => {
      let next: readonly PickItem[] = item ? [item] : [];
      if (opts.multi && item) next = target.picked.some((p) => p.id === item.id) ? target.picked.filter((p) => p.id !== item.id) : [...target.picked, item];
      target.save({ field, value: pickedValue(kind, next) }, next);
    },
  };
}

/** A field's editor in place: a pick list for a field that takes values
 * a person picks, a typed value for one text, number or date; none for a
 * field not settable, a date and time, a read-only value, several typed
 * values (which one line of text cannot hold apart) or long text, which
 * its section or the move's form edits as Markdown. */
export function fieldEditor(f: FieldView, target: EditorTarget): FieldEditor | null {
  if (!f.editable) return null;
  switch (f.kind) {
    case 'option':
    case 'user':
      return pickEditor(f.id, f.kind, target, { multi: f.list, clear: f.required ? undefined : 'None' });
    case 'text':
    case 'number':
    case 'date':
      if (f.list) return null;
      return {
        kind: 'type',
        input: f.kind,
        text: target.text,
        onSave: (text) => target.save(text ? { field: f.id, typed: [text] } : { field: f.id, value: {} as FieldValueView }, []),
      };
    default:
      return null;
  }
}

/** The target of a field the ticket holds, whose edit is saved at once. */
export function ticketTarget(value: FieldValueView, choices: EditorTarget['choices'], save: (edit: FieldEditInput) => void): EditorTarget {
  return { picked: pickedOf(value), text: fieldText(value), choices, save: (edit) => save(edit) };
}
