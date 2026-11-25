<script lang="ts" module>
  /** A connection the finder looks for tickets on, on its computer. */
  export interface FinderSource {
    readonly backend: string;
    readonly connection: string;
    /** Names the connection beside its tickets when there are several. */
    readonly name: string;
  }

  /** A ticket the finder found or was given to offer. */
  export interface FoundTicket {
    readonly backend: string;
    readonly connection: string;
    readonly issue: string;
    /** Empty for a ticket the tracker did not name. */
    readonly key: string;
    readonly title: string;
  }

  // A key as a person types it: letters and digits, a dash, a number.
  const KEY_SHAPE = /^[A-Za-z][A-Za-z0-9_]*-\d+$/;

  /** Whether `text` is shaped like a ticket key ("WEB-12", "web-12"). */
  export function isKeyShaped(text: string): boolean {
    return KEY_SHAPE.test(text.trim());
  }
</script>

<script lang="ts">
  // Finds a ticket by key or by words of its title, on one connection or
  // several, at its control. Text shaped like a key is looked up
  // (`TrackerLookup`); other text is searched (`TrackerSearch`, at most
  // `limit` a connection). Before anything is typed the list offers
  // `before` (the tickets the caller has at hand), or nothing. It reads
  // only through its own calls, each pinned to its connection's computer.
  //
  // The picker it sits on (TicketPicker.svelte) keeps the search rules:
  // one read per pause in typing, an answer or failure shown only while
  // the box holds the text it was asked for, and a failure shown in the
  // list. With several connections, every connection is asked and the
  // first failure is what the list says.
  import TicketPicker, { type PickItem, type PickList } from './TicketPicker.svelte';
  import { TrackerLookup, TrackerSearch } from '../../stores/bindings';
  import { withBackendTarget } from '../../transport/backends';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    sources: readonly FinderSource[];
    /** Offered before anything is typed. */
    before?: readonly FoundTicket[];
    /** The most tickets a search lists from each connection. */
    limit?: number;
    onPick: (ticket: FoundTicket) => void;
    onClose: () => void;
  }

  let { anchor, open, sources, before = [], limit = 10, onPick, onClose }: Props = $props();

  // The tickets the list shows now, by the ID of their row.
  const byItem = new Map<string, FoundTicket>();
  const NONE: ReadonlySet<string> = new Set();

  function item(t: FoundTicket): PickItem {
    const id = `${t.backend}\n${t.connection}\n${t.issue}`;
    byItem.set(id, t);
    const source = sources.length > 1 ? sources.find((s) => s.backend === t.backend && s.connection === t.connection)?.name : undefined;
    return { id, name: t.key || `ID ${t.issue}`, description: [t.title, source].filter(Boolean).join(' · ') };
  }

  async function find(source: FinderSource, text: string): Promise<{ found: FoundTicket[]; more: boolean }> {
    const { backend, connection } = source;
    const ticket = (card: { id: string; key: string; title: string }): FoundTicket => ({ backend, connection, issue: card.id, key: card.key, title: card.title });
    if (isKeyShaped(text)) {
      const view = await withBackendTarget(backend, () => TrackerLookup(connection, [text.toUpperCase()]));
      return { found: (view.found ?? []).map((m) => ticket(m.card)), more: false };
    }
    const page = await withBackendTarget(backend, () => TrackerSearch(connection, { text, limit }));
    return { found: (page.cards ?? []).map(ticket), more: !!page.next };
  }

  async function load(text: string): Promise<PickList> {
    if (!text) {
      byItem.clear();
      // More: typed text is always asked of the tracker.
      return { items: before.map(item), more: true };
    }
    const answers = await Promise.all(sources.map((s) => find(s, text)));
    return { items: answers.flatMap((a) => a.found).map(item), more: answers.some((a) => a.more) };
  }

  function pick(picked: PickItem | null): void {
    const t = picked ? byItem.get(picked.id) : undefined;
    if (t) onPick(t);
  }
</script>

<TicketPicker {anchor} {open} label="Ticket" {load} checked={NONE} onPick={pick} {onClose} />
