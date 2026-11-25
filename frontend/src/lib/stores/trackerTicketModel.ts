// The ticket screen's rules that hold no state (docs/specs/issues.md,
// "Ticket"): which comments and changes are mounted, the order of the
// rail's fields, which fields are sections, how linked tickets group, and
// the row of the board the ticket shows its points from. What a write's
// answer means is trackerWriteOutcome.ts's. The stores and the components
// call these; each rule is here once.

import type {
  CommentView,
  DetailView,
  EventView,
  FieldValueView,
  FieldView,
  LinkView,
  TicketRowView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** Comments shown on open, and how many more each press of the control
 * mounts. */
export const COMMENTS_FIRST = 5;
export const COMMENTS_STEP = 20;
/** Changes of the history mounted on open, and per press. */
export const HISTORY_FIRST = 20;
export const HISTORY_STEP = 20;

/** The ticket's comments held, newest first: the view's first page, then
 * the earlier pages a person loaded in order, each comment once (a page
 * read again after a delete can repeat a newer page's last comment). */
export function heldComments(detail: DetailView, pages: readonly { readonly comments: readonly CommentView[] }[]): readonly CommentView[] {
  const first = detail.comments ?? [];
  if (pages.length === 0) return first;
  const seen = new Set(first.map((c) => c.id));
  const out = [...first];
  for (const page of pages) {
    for (const c of page.comments) {
      if (seen.has(c.id)) continue;
      seen.add(c.id);
      out.push(c);
    }
  }
  return out;
}

export type CommentOrder = 'oldest' | 'newest';

/** The comments mounted, in the order they read, and the earlier ones not
 * mounted: the newest `shown` of the held (newest first). */
export function commentWindow(held: readonly CommentView[], shown: number, order: CommentOrder): { readonly comments: readonly CommentView[]; readonly hidden: number } {
  const take = held.slice(0, Math.max(0, shown));
  return { comments: order === 'newest' ? take : take.reverse(), hidden: held.length - take.length };
}

/** The ticket's changes newest first: ordered once per view, and
 * `historyWindow` slices them on each reveal. */
export function newestHistory(history: readonly EventView[]): readonly EventView[] {
  return [...history].sort((a, b) => b.at - a.at);
}

/** The changes mounted from `newest` (newestHistory), and how many older
 * ones are not. */
export function historyWindow(newest: readonly EventView[], shown: number): { readonly events: readonly EventView[]; readonly hidden: number } {
  const take = newest.slice(0, Math.max(0, shown));
  return { events: take, hidden: newest.length - take.length };
}

/** The ticket's links grouped by how the ticket relates to each issue,
 * the groups in the order the tracker lists their first link. */
export function linkGroups(links: readonly LinkView[]): readonly { readonly relation: string; readonly links: readonly LinkView[] }[] {
  const groups = new Map<string, LinkView[]>();
  for (const l of links) {
    const group = groups.get(l.relation);
    if (group) group.push(l);
    else groups.set(l.relation, [l]);
  }
  return [...groups].map(([relation, of]) => ({ relation, links: of }));
}

/** Whether a field holds a value. */
export function isFilled(value: FieldValueView): boolean {
  return !!value.kind;
}

/** A long text field: its own section of the content, not a rail row. */
export function isSection(field: FieldView): boolean {
  return field.kind === 'rich_text';
}

/** The rail's further fields (behind its control): every field that is not
 * a section, filled first, each group in the tracker's order, narrowed to
 * those whose name holds `find`. Pins plug in here: a project's pinned
 * fields will leave this list for the rail's first group. */
export function moreFields(fields: readonly FieldView[], find: string): readonly FieldView[] {
  const words = find.trim().toLowerCase();
  const rows = fields.filter((f) => !isSection(f) && (!words || f.name.toLowerCase().includes(words)));
  return [...rows.filter((f) => isFilled(f.value)), ...rows.filter((f) => !isFilled(f.value))];
}

/** The ticket's long text sections besides the description, in the
 * tracker's order. Pins plug in here: a project's pinned sections will
 * show open, the rest in the one line that names them. */
export function sectionFields(fields: readonly FieldView[]): readonly FieldView[] {
  return fields.filter(isSection);
}

/** The long text the ticket shows for field `id`, as Markdown: the
 * description's own, which the view carries apart from its fields, or
 * the field's. A section and a move's form both edit from it. */
export function shownText(detail: DetailView, id: string): string {
  if (id === 'description') return detail.description;
  const f = detail.fields.find((o) => o.id === id);
  return f ? fieldText(f.value) : '';
}

/** A field's value as text, for a row and for a section's Markdown. */
export function fieldText(value: FieldValueView): string {
  switch (value.kind) {
    case 'text':
    case 'rich_text':
    case 'read_only':
      return (value.texts ?? []).join(', ');
    case 'number':
      return (value.numbers ?? []).join(', ');
    case 'date':
      return (value.dates ?? []).join(', ');
    case 'date_time':
      return (value.times ?? []).map((ms) => new Date(ms).toLocaleString()).join(', ');
    case 'user':
    case 'option':
      return (value.choices ?? []).map((c) => c.name || c.id).join(', ');
    default:
      return '';
  }
}

/** The row the ticket shows its points from: the board it was opened from,
 * else the first the mirror holds; null when no board shows the issue. */
export function ticketRow(rows: readonly TicketRowView[] | null | undefined, board: string | null): TicketRowView | null {
  if (!rows || rows.length === 0) return null;
  return rows.find((r) => r.board === board) ?? rows[0];
}
