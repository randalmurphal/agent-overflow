// The ticket screen's rules that hold no state (docs/specs/issues.md,
// "Ticket"): which comments and changes are mounted, the order of the
// rail's fields, which fields are sections, how linked tickets group and
// which side a new link sends, what a poker round's votes add up to,
// which attachment a file in the text names, and the row of the board the
// ticket shows its points from. What a write's answer means is
// trackerWriteOutcome.ts's. The stores and the components call these;
// each rule is here once.

import type {
  AttachmentView,
  CommentView,
  DetailView,
  EventView,
  FieldValueView,
  FieldView,
  LinkRequest,
  LinkTypeView,
  LinkView,
  PokerVoteView,
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
 * mounted: the newest `shown` of the held (newest first), widened to the
 * oldest comment whose edit a person left unsent (`drafted`), so that
 * text is never hidden behind the reveal control and the window stays
 * one run. */
export function commentWindow(
  held: readonly CommentView[],
  shown: number,
  order: CommentOrder,
  drafted: ReadonlySet<string>,
): { readonly comments: readonly CommentView[]; readonly hidden: number } {
  let count = Math.max(0, shown);
  if (drafted.size > 0) {
    for (let i = held.length - 1; i >= count; i--) {
      if (drafted.has(held[i].id)) {
        count = i + 1;
        break;
      }
    }
  }
  const take = held.slice(0, count);
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

/** One way the ticket can relate to another: a link type's phrase from
 * one of its sides. `outward` is whether the ticket is the type's outward
 * issue ("blocks") or its inward one ("is blocked by"). */
export interface LinkRelation {
  readonly type: string;
  readonly phrase: string;
  readonly outward: boolean;
}

/** Every relation the link types offer, in the tracker's order: each
 * type's outward phrase, then its inward one when it reads differently (a
 * "relates to" type is one relation either way). */
export function linkRelations(types: readonly LinkTypeView[]): readonly LinkRelation[] {
  const out: LinkRelation[] = [];
  for (const t of types) {
    out.push({ type: t.id, phrase: t.outward, outward: true });
    if (t.inward !== t.outward) out.push({ type: t.id, phrase: t.inward, outward: false });
  }
  return out;
}

/** The link that makes `ticket` relate to `other` as `relation` reads,
 * both named by issue ID. */
export function linkRequest(relation: LinkRelation, ticket: string, other: string): LinkRequest {
  return relation.outward
    ? { type: relation.type, outward: ticket, inward: other }
    : { type: relation.type, outward: other, inward: ticket };
}

/** The values the votes show, each with how many chose it, most votes
 * first and the lower value first among equals. A vote whose value the
 * read does not show counts toward none. */
export function pokerResult(votes: readonly PokerVoteView[]): readonly { readonly value: number; readonly count: number }[] {
  const counts = new Map<number, number>();
  for (const v of votes) {
    if (typeof v.value === 'number') counts.set(v.value, (counts.get(v.value) ?? 0) + 1);
  }
  return [...counts].map(([value, count]) => ({ value, count })).sort((a, b) => b.count - a.count || a.value - b.value);
}

/** The largest image the ticket's text shows in place; a larger one stays
 * a chip with its size and Save, so one image cannot take more than a
 * quarter of the media cache (utils/mediaBlobCache.ts). */
export const INLINE_IMAGE_MAX_BYTES = 16 * 1024 * 1024;

/** The attachment a file in the ticket's text names: the one attachment
 * whose name is the text's, or null when none or several have it. The
 * tracker's text names a file by its media ID, which no attachment of the
 * view carries, and by its name. */
export function mediaAttachment(attachments: readonly AttachmentView[], name: string): AttachmentView | null {
  let found: AttachmentView | null = null;
  for (const a of attachments) {
    if (a.name !== name) continue;
    if (found) return null;
    found = a;
  }
  return found;
}

/** Whether the ticket's text shows the attachment as an image in place. */
export function showsInline(attachment: AttachmentView): boolean {
  return (attachment.mime_type ?? '').startsWith('image/') && attachment.size <= INLINE_IMAGE_MAX_BYTES;
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
