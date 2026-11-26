// How a ticket field is changed in place (docs/specs/issues.md, "Ticket"
// and "Board"): the one rule for which fields take an editor, what a pick
// or a typed value sends, and how a field of several values takes a pick.
// The ticket and a Board card save an edit at once (`cardEditor`, the
// card's four fields as both make them); a move's form collects them for
// the move (TicketMoveForm.svelte). Each passes its own target.

import type {
  CardView,
  ChoiceListView,
  FieldEditInput,
  FieldValueView,
  FieldView,
} from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import { fieldText } from '../../stores/trackerTicketModel';
import {
  assignableUsers,
  editTicketFields,
  fieldChoices,
  fieldTarget,
  setTicketPoints,
  type TicketWriteContext,
} from '../../stores/trackerTicketWrites.svelte';
import type { FieldEditor, TypingHold } from './TicketField.svelte';
import type { PickItem } from './TicketPicker.svelte';

/** The fields a Board card changes from its marks. */
export type CardEditField = 'type' | 'priority' | 'points' | 'assignee';

// The names the card's fields go by, for a refusal naming them.
const CARD_FIELD_NAMES: Readonly<Record<string, string>> = {
  status: 'Status', assignee: 'Assignee', priority: 'Priority', type: 'Type', points: 'Points', description: 'Description', title: 'Title',
};

/** A card field's name as a person reads it; the ID for another field. */
export function cardFieldName(id: string): string {
  return CARD_FIELD_NAMES[id] ?? id;
}

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
  /** Holds the text being typed in a typed field, for a form whose field
   * may go before it is saved. */
  readonly typing?: TypingHold;
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
        typing: target.typing,
      };
    default:
      return null;
  }
}

/** The target of a field the ticket holds, whose edit is saved at once. */
export function ticketTarget(value: FieldValueView, choices: EditorTarget['choices'], save: (edit: FieldEditInput) => void): EditorTarget {
  return { picked: pickedOf(value), text: fieldText(value), choices, save: (edit) => save(edit) };
}

/** The estimate a ticket or a card shows, on the board it is set on. */
export interface PointsRow {
  readonly board: string;
  /** points, time (seconds) or none. */
  readonly unit: string;
  readonly estimate?: number | null;
}

/** The label of an estimate in `unit`. */
export function pointsLabel(unit: string): string {
  return unit === 'time' ? 'Estimate (hours)' : 'Points';
}

/** Sets the estimate on `row`'s board: points as they are, hours as the
 * seconds the tracker takes. */
export function saveEstimate(ctx: TicketWriteContext, row: PointsRow, value: number | null): void {
  void setTicketPoints(ctx, row.board, value === null ? null : row.unit === 'time' ? Math.round(value * 3600) : value);
}

/** An option value holding `c`, or the empty value. */
function choiceValue(c: { id: string; name?: string } | null | undefined): FieldValueView {
  return (c ? { kind: 'option', choices: [c] } : {}) as FieldValueView;
}

/** A field the ticket holds, whose edit is saved at once on `field`'s target. */
function savedTarget(ctx: TicketWriteContext, field: string, value: FieldValueView): EditorTarget {
  return ticketTarget(value, (text) => fieldChoices(ctx, field, text), (edit) => {
    void editTicketFields(ctx, fieldTarget(field), [edit]);
  });
}

/**
 * The editor of one of a card's fields, as the ticket screen and a Board
 * card make it, each edit saved at once: the assignee from the people who
 * may be assigned (Unassigned clears it), the priority and the type from
 * the values the ticket allows, and the estimate typed, in hours on a
 * board that estimates time. None for the type on a tracker without
 * types (`typed`), or for the estimate on a board without one.
 */
export function cardEditor(ctx: TicketWriteContext, card: CardView, field: CardEditField, points: PointsRow | null, typed: boolean): FieldEditor | null {
  switch (field) {
    case 'assignee':
      return pickEditor('assignee', 'user', {
        ...savedTarget(ctx, 'assignee', choiceValue(card.assignee)),
        choices: async (text) => {
          const got = await assignableUsers(ctx, text);
          return { choices: (got.users ?? []).map((u) => ({ id: u.id, name: u.name })), more: got.more };
        },
      }, { clear: 'Unassigned' });
    case 'priority':
      return pickEditor('priority', 'option', savedTarget(ctx, 'priority', choiceValue(card.priority)));
    case 'type':
      return typed ? pickEditor('type', 'option', savedTarget(ctx, 'type', choiceValue(card.type))) : null;
    case 'points': {
      if (!points || points.unit === 'none' || points.unit === '') return null;
      const row = points;
      return {
        kind: 'type',
        input: 'number',
        text: row.estimate == null ? '' : String(row.unit === 'time' ? row.estimate / 3600 : row.estimate),
        onSave: (text: string) => saveEstimate(ctx, row, text === '' ? null : Number(text)),
      };
    }
  }
}
