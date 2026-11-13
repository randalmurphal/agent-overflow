// The Details section's picker: one row's searchable multi-select
// (reviewers, assignees or labels). Its query and selection live in the
// review state, so a remounted overview keeps them and neither applies
// nor loses them. Results load when it opens and as the reader types
// (debounced); a query change drops the older results at once, keeping
// the checked entries listed, and an answer to an older query is
// dropped. Closing it, by any route, the pane's closing included,
// applies the selection as one change to the PR it opened on: the add
// and remove sets against what the row showed when it opened; nothing is
// written when they are both empty. The PR changing under it discards
// it.

import { SearchPRCandidates } from './bindings';
import { captureError } from './errorReports.svelte';
import type { PRManageError } from './prManage.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { PRLabel, PRPerson } from '../types/models';
import { labelKey, personKey, pickerQuery, selectionChange, type PRManageRow } from '../utils/prManage';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { userFacingError } from '../utils/userFacingError';

/** Typing settles this long before the search runs. */
export const PICKER_SEARCH_DEBOUNCE_MS = 250;

export interface PRPickerChange {
  readonly people: { add: PRPerson[]; remove: PRPerson[] };
  readonly labels: { add: PRLabel[]; remove: PRLabel[] };
}

/** The PR a picker opened on: its reference and computer-scoped key. */
export interface PRManageTarget {
  readonly ref: PRRef;
  readonly key: string;
}

export interface PRPickerHost {
  /** The PR on screen now, or null. */
  readonly target: PRManageTarget | null;
  /** The identity on screen now (the target's key), or null. */
  readonly identity: string | null;
  readonly backend: BackendKey;
  /** Writes the change to `target`; where a failure shows is decided
   * when the answer arrives. */
  apply(row: PRManageRow, change: PRPickerChange, target: PRManageTarget): Promise<void>;
  /** The row's picker opened. */
  opened(row: PRManageRow): void;
}

export interface PRPickerOption {
  readonly key: string;
  readonly person?: PRPerson;
  readonly label?: PRLabel;
  readonly checked: boolean;
  /** Shown checked and cannot be unchecked here (a GitHub reviewer
   * whose review is in: only a pending request can be withdrawn). */
  readonly locked: boolean;
}

/** What the row showed when its picker opened. */
export interface PRPickerOpen {
  readonly people?: readonly PRPerson[];
  readonly labels?: readonly PRLabel[];
  /** Keys shown checked that cannot be unchecked. */
  readonly locked?: readonly string[];
  /** Offer teams the search returns (GitHub reviewers in an organization). */
  readonly teams?: boolean;
}

export interface PRPickerView {
  readonly row: PRManageRow | null;
  readonly query: string;
  readonly options: readonly PRPickerOption[];
  readonly loading: boolean;
  /** The forge has more than the first page: the reader should narrow. */
  readonly more: boolean;
  readonly error: PRManageError | null;
  open(row: PRManageRow, shown: PRPickerOpen): void;
  setQuery(text: string): void;
  toggle(key: string): void;
  /** Closes the picker and applies its change. */
  close(): Promise<void>;
  /** Closes the picker without writing (the PR changed). */
  discard(): void;
  /** The pane is closing: applies the open change, then clears. */
  dispose(): void;
}

type Item = { person: PRPerson; label?: undefined } | { label: PRLabel; person?: undefined };

function itemKey(item: Item): string {
  return item.person ? personKey(item.person) : labelKey(item.label);
}

export function createPRPickerState(host: PRPickerHost): PRPickerView {
  let openRow = $state<PRManageRow | null>(null);
  let target: PRManageTarget | null = null;
  let targetKey = $state<string | null>(null);
  // A picker shows only on the PR it opened on.
  const row = $derived(openRow !== null && targetKey === host.identity ? openRow : null);
  let query = $state('');
  let teams = false;
  // What the row showed (initial), what is selected now, and every item
  // the reader has seen checked in this session, in a stable order: the
  // empty query lists those first so an unchecked one stays in reach.
  let initial = $state.raw<ReadonlyMap<string, Item>>(new Map());
  let selected = $state.raw<ReadonlyMap<string, Item>>(new Map());
  let known = $state.raw<ReadonlyMap<string, Item>>(new Map());
  let locked = $state.raw<ReadonlySet<string>>(new Set());
  let results = $state.raw<readonly Item[]>([]);
  let more = $state(false);
  let loading = $state(false);
  let error = $state<PRManageError | null>(null);
  // Each search's generation; an answer whose generation is not the
  // latest is dropped.
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  function clearTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  async function search(forRow: PRManageRow, text: string, mine: number): Promise<void> {
    const ref = target?.ref;
    if (!ref || mine !== generation) return;
    loading = true;
    try {
      const answer = await withBackendTarget(host.backend, () => SearchPRCandidates(prReferenceWire(ref), forRow, pickerQuery(text)));
      if (mine !== generation || row !== forRow) return;
      const items: Item[] = forRow === 'labels'
        ? (answer.labels ?? []).map((label) => ({ label }))
        : (answer.people ?? []).filter((person) => teams || !person.team).map((person) => ({ person }));
      results = items;
      more = answer.more;
      error = null;
    } catch (err) {
      if (mine !== generation || row !== forRow) return;
      results = [];
      more = false;
      error = { message: userFacingError(err), captured: captureError(err) };
    } finally {
      if (mine === generation) loading = false;
    }
  }

  const options = $derived.by<PRPickerOption[]>(() => {
    if (row === null) return [];
    const out: PRPickerOption[] = [];
    const listed = new Set<string>();
    const push = (key: string, item: Item) => {
      if (listed.has(key)) return;
      listed.add(key);
      out.push({ key, person: item.person, label: item.label, checked: selected.has(key), locked: locked.has(key) });
    };
    if (pickerQuery(query) === '') for (const [key, item] of known) push(key, item);
    // While a new query's answer is awaited the old results are gone;
    // what is checked stays in reach.
    else if (loading) for (const [key, item] of selected) push(key, item);
    for (const item of results) push(itemKey(item), item);
    return out;
  });

  function reset(): void {
    clearTimer();
    generation += 1;
    openRow = null;
    target = null;
    targetKey = null;
    query = '';
    initial = new Map();
    selected = new Map();
    known = new Map();
    locked = new Set();
    results = [];
    more = false;
    loading = false;
    error = null;
  }

  // Closes the picker and writes its change to the PR it opened on.
  async function apply(): Promise<void> {
    const closingRow = row;
    const on = target;
    if (closingRow === null || on === null) {
      reset();
      return;
    }
    const change = selectionChange(initial, selected);
    reset();
    if (change.add.length === 0 && change.remove.length === 0) return;
    const people = { add: change.add.flatMap((item) => item.person ?? []), remove: change.remove.flatMap((item) => item.person ?? []) };
    const labels = { add: change.add.flatMap((item) => item.label ?? []), remove: change.remove.flatMap((item) => item.label ?? []) };
    await host.apply(closingRow, { people, labels }, on);
  }

  return {
    get row() { return row; },
    get query() { return query; },
    get options() { return options; },
    get loading() { return loading; },
    get more() { return more; },
    get error() { return error; },
    open(next: PRManageRow, shown: PRPickerOpen): void {
      const on = host.target;
      if (!on) return;
      reset();
      host.opened(next);
      openRow = next;
      target = on;
      targetKey = on.key;
      teams = !!shown.teams;
      const items: Item[] = next === 'labels'
        ? (shown.labels ?? []).map((label) => ({ label }))
        : (shown.people ?? []).map((person) => ({ person }));
      const map = new Map(items.map((item) => [itemKey(item), item]));
      initial = map;
      selected = map;
      known = map;
      locked = new Set(shown.locked ?? []);
      void search(next, '', ++generation);
    },
    setQuery(text: string): void {
      if (row === null) return;
      query = text;
      clearTimer();
      // The older answer is dropped now, not when the debounce ends: its
      // results are not this query's and cannot be picked.
      const mine = ++generation;
      results = [];
      more = false;
      error = null;
      loading = true;
      const forRow = row;
      timer = setTimeout(() => {
        timer = null;
        void search(forRow, text, mine);
      }, PICKER_SEARCH_DEBOUNCE_MS);
    },
    toggle(key: string): void {
      if (row === null || locked.has(key)) return;
      const next = new Map(selected);
      if (next.has(key)) {
        next.delete(key);
      } else {
        const item = known.get(key) ?? results.find((each) => itemKey(each) === key);
        if (!item) return;
        next.set(key, item);
        if (!known.has(key)) known = new Map([...known, [key, item]]);
      }
      selected = next;
    },
    close(): Promise<void> {
      return apply();
    },
    discard(): void {
      reset();
    },
    dispose(): void {
      void apply();
    },
  };
}
