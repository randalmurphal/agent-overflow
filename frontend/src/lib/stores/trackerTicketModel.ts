// The ticket screen's rules that hold no state (docs/specs/issues.md,
// "Ticket"): which comments and changes are mounted, the order of the
// rail's fields and of the sections by the project's pins and how a pin
// changes them, which fields are sections, how linked tickets group and
// which side a new link sends, what a poker round's votes add up to,
// which attachment a file in the text names and which the app previews,
// the row of the board the ticket shows its points from, and how its
// sprints are named. What a write's answer means is
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
  Setup,
  SitePins,
  TicketRowView,
  TicketSprintView,
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

/** A revealed round's choice as the poker section and the ticket menu
 * offer it: "Set estimate to 5 hours" and "(2 votes)". */
export function pokerEstimateChoice(r: { readonly value: number; readonly count: number }, hours: boolean): { readonly action: string; readonly votes: string } {
  const unit = hours ? (r.value === 1 ? ' hour' : ' hours') : '';
  return { action: `Set estimate to ${r.value}${unit}`, votes: `(${r.count} ${r.count === 1 ? 'vote' : 'votes'})` };
}

/** The largest image the app previews, in the ticket's text or from its
 * attachment row; a larger one is saved or opened in the tracker, so one
 * image cannot take more than a quarter of the media cache
 * (utils/mediaBlobCache.ts). */
export const PREVIEW_MAX_BYTES = 16 * 1024 * 1024;

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

/** Whether the app previews the attachment: the one admission rule for
 * an image in the ticket's text and for Open on its row. */
export function previewsInApp(attachment: AttachmentView): boolean {
  return (attachment.mime_type ?? '').startsWith('image/') && attachment.size <= PREVIEW_MAX_BYTES;
}

/** Whether a field holds a value. */
export function isFilled(value: FieldValueView): boolean {
  return !!value.kind;
}

/** A long text field: its own section of the content, not a rail row. */
export function isSection(field: FieldView): boolean {
  return field.kind === 'rich_text';
}

/** What a ticket of one tracker site shows first for the project, as its
 * setup holds it: each list null while it is not set. */
export interface TicketPins {
  readonly fields: readonly string[] | null;
  readonly sections: readonly string[] | null;
}

/** The pins `setup` holds for a ticket of `tracker` `site`. */
export function ticketPinsOf(setup: Setup | null | undefined, tracker: string, site: string): TicketPins {
  const entry = setup?.ticket_pins?.find((p) => p.tracker === tracker && p.site === site);
  return { fields: entry?.fields ?? null, sections: entry?.sections ?? null };
}

/** The sections a ticket may have a place for besides its long text
 * fields, in the ticket's order. */
const BUILT_IN_SECTIONS: readonly string[] = ['description', 'links', 'attachments'];

/** The sections a ticket shows open while its project pins none: those
 * of BUILT_IN_SECTIONS it has a place for (`attachments`: a tracker that
 * stores files, or a ticket that has some), every long text field in the
 * line that names them. The layout and a first pin read this one list. */
function defaultPinnedSections(attachments: boolean): readonly string[] {
  return attachments ? BUILT_IN_SECTIONS : BUILT_IN_SECTIONS.filter((id) => id !== 'attachments');
}

/** The rail's fields past the card's: those the project pins, in pin
 * order, and the rest behind its control (every field that is not a
 * section, filled first, each group in the tracker's order, narrowed to
 * those whose name holds `find`); `missing` is the pinned IDs this ticket
 * has no field of, which the control lists so they can be unpinned. */
export function railFields(fields: readonly FieldView[], pinned: readonly string[] | null, find: string): {
  readonly pinned: readonly FieldView[];
  readonly more: readonly FieldView[];
  readonly missing: readonly string[];
} {
  const pins = new Set(pinned ?? []);
  const byId = new Map(fields.filter((f) => !isSection(f)).map((f) => [f.id, f]));
  const shown = (pinned ?? []).flatMap((id) => byId.get(id) ?? []);
  const words = find.trim().toLowerCase();
  const rows = fields.filter((f) => !isSection(f) && !pins.has(f.id) && (!words || f.name.toLowerCase().includes(words)));
  return {
    pinned: shown,
    more: [...rows.filter((f) => isFilled(f.value)), ...rows.filter((f) => !isFilled(f.value))],
    missing: (pinned ?? []).filter((id) => !byId.has(id)),
  };
}

/** The ticket's sections by the project's pins: `shown` open, in pin
 * order; `folded` in the one line that names them, in the ticket's order
 * (description, its long text fields in the tracker's order, linked
 * tickets, attachments where the ticket has them: `attachments`);
 * `missing` the pinned long text fields this ticket has none of. Unset
 * pins pin defaultPinnedSections. */
export function ticketSections(fields: readonly FieldView[], pinned: readonly string[] | null, attachments: boolean): {
  readonly shown: readonly string[];
  readonly folded: readonly string[];
  readonly missing: readonly string[];
} {
  const order = ['description', ...fields.filter(isSection).map((f) => f.id), 'links', ...(attachments ? ['attachments'] : [])];
  const has = new Set(order);
  const pins = pinned ?? defaultPinnedSections(attachments);
  const pinnedSet = new Set(pins);
  return {
    shown: pins.filter((id) => has.has(id)),
    folded: order.filter((id) => !pinnedSet.has(id)),
    missing: pins.filter((id) => !has.has(id) && !BUILT_IN_SECTIONS.includes(id)),
  };
}

/** `setup` with `id` pinned or unpinned in the `kind` list of `tracker`
 * `site`: a pin goes last, and a list not set starts from what the ticket
 * shows unpinned (no fields; defaultPinnedSections, by whether it has a
 * place for `attachments`). */
export function withTicketPin(setup: Setup, tracker: string, site: string, kind: 'fields' | 'sections', id: string, pin: boolean, attachments: boolean): Setup {
  const entries = setup.ticket_pins ?? [];
  const at = entries.findIndex((p) => p.tracker === tracker && p.site === site);
  const entry: SitePins = at >= 0 ? entries[at] : { tracker, site };
  const list = entry[kind] ?? (kind === 'sections' ? defaultPinnedSections(attachments) : []);
  const next = pin ? (list.includes(id) ? [...list] : [...list, id]) : list.filter((p) => p !== id);
  const others = entries.filter((_, i) => i !== at);
  return { ...setup, ticket_pins: [...others, { ...entry, [kind]: next }] };
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

/** The ticket's sprints as its Sprint field says them: each by name, in
 * the order the view gives (`DetailView.sprints`: active, future, closed,
 * then those no board of the ticket holds), the last said in words, since
 * a sprint's ID means nothing to a person. */
export function sprintNames(sprints: readonly TicketSprintView[]): string {
  const named = sprints.filter((sp) => sp.name).map((sp) => sp.name!);
  const other = sprints.length - named.length;
  if (other === 1) named.push('a sprint on another board');
  else if (other > 1) named.push(`${other} sprints on other boards`);
  return named.join(', ');
}
