// The one owner of the agent roster settings page's state
// (AgentProfilesSettings): both lists' drafts, the checks of the profiles,
// the Restore waiting on its answer, and the order every change is applied
// in. The page reads it through readonly views and changes it only through
// one method per event, so no handler can write state the others order
// against.
//
// Ordering:
// - A save writes only if nothing replaced its list's draft while it was
//   checking (a change saved elsewhere, Restore's answer, taking the other
//   version, or a newer save). A save dropped by a change saved elsewhere
//   saves the reconciled draft instead.
// - A check's results show only if no newer check began meanwhile.
// - A Restore acts on its answer only if nothing newer happened while it
//   waited: any change the person makes, a second Restore or the page
//   closing retires it silently, and a change saved elsewhere retires it
//   and says so. A save that finishes while Restore waits writes, and
//   Restore's write follows it in the computer's queue.
// - Each list has at most one write in flight. A newer save of the list
//   waits as the newest wanted write and is sent when the one in flight
//   settles; Restore's write of both lists waits until neither has one.
// - The store says how each write ended (RosterWrite): saved, with the
//   computer's lists after it, which marks the write's entries saved;
//   refused, which leaves the person's edits on the page, unsaved, for the
//   next commit to send again; or dropped, when the computer went away. A
//   saved list the page did not know, in that answer or shown by the store
//   while no write of the list is in flight, changed elsewhere.
import { noteProblems, profileProblems, trimNote, trimProfile } from './agentRoster';
import {
  draftOf,
  keepMine,
  markSaved,
  marked,
  reconcile,
  remove,
  sameList,
  takeTheirs,
  unsaved,
  type Draft,
  type DraftEntry,
} from './rosterDraft';
import type { AgentNote, AgentProfile } from '../../types/settings';

/** What a computer makes of one profile (CheckAgentProfiles). */
export type ProfileCheck = { problem?: string; missing?: string; runs?: string; unreadable?: string[]; family?: boolean };

/** A roster the computer starts with (AgentRosterSeeds). */
export interface Roster {
  profiles: AgentProfile[];
  notes: AgentNote[];
}

export type RosterList = 'profiles' | 'notes';

export interface RosterSaved {
  agentProfiles: readonly AgentProfile[];
  agentNotes: readonly AgentNote[];
}

export interface RosterPatch {
  agentProfiles?: AgentProfile[];
  agentNotes?: AgentNote[];
}

/**
 * How a write ended, as the settings store reports it (SettingsWrite):
 * `saved` and `refused` carry the computer's saved lists after it; `local`
 * says the computer stores no roster.
 */
export type RosterWrite =
  | { status: 'local' }
  | { status: 'saved'; saved: RosterSaved }
  | { status: 'refused'; saved: RosterSaved }
  | { status: 'dropped' };

/** What the page's computer and store do for the owner. */
export interface RosterDeps {
  /** The saved lists as the store shows them now. */
  saved(): RosterSaved;
  /** What the computer makes of each profile; throws when it cannot tell. */
  check(profiles: AgentProfile[]): Promise<ProfileCheck[]>;
  /** Why a check that threw could not tell. */
  checkFailure(err: unknown): string;
  /** The roster the computer starts with; null when its release has none. */
  seeds(): Promise<Roster | null>;
  /** Why a computer with no starting roster cannot restore one. */
  noSeeds(): string;
  /** Reports that the starting roster could not be read. */
  seedsFailed(err: unknown): void;
  /** Writes through the store, which shows the patch at once, and says how it ended. */
  write(patch: RosterPatch): Promise<RosterWrite>;
}

type ReadonlyEntry<T> = {
  readonly value: Readonly<T>;
  readonly origin: Readonly<T> | null;
  readonly theirs?: Readonly<T> | null;
};

export type ReadonlyDraft<T> = {
  readonly entries: readonly ReadonlyEntry<T>[];
  readonly removed: readonly ReadonlyEntry<T>[];
};

/** Said where a Restore's refusal is, when a change saved elsewhere retired it. */
export const RESTORE_OVERTAKEN =
  'The roster changed elsewhere while Restore waited for the starting roster, so Restore changed nothing.';

/** The key a profile's check is filed under: what the check reads of it. */
export function checkKey(profile: Readonly<AgentProfile>): string {
  return JSON.stringify([profile.provider, profile.model.trim(), profile.effort]);
}

// A profile's saved counterpart, when it changed elsewhere too: the one with
// its name, which is unique in the list.
const sameProfile = (origin: AgentProfile, entry: AgentProfile) =>
  origin.name.toLowerCase() === entry.name.toLowerCase();

/**
 * One list's part of a write: what the save took and what it sends. `pair`
 * marks Restore's write, whose two parts are sent together.
 */
interface Sent<T> {
  list: RosterList;
  entries: DraftEntry<T>[];
  removed: DraftEntry<T>[];
  sent: T[];
  pair?: boolean;
}

type AnySent = Sent<AgentProfile> | Sent<AgentNote>;

export class RosterEditor {
  readonly #deps: RosterDeps;
  #profiles = $state<Draft<AgentProfile>>({ entries: [], removed: [] });
  #notes = $state<Draft<AgentNote>>({ entries: [], removed: [] });
  #checks = $state<Record<string, ProfileCheck>>({});
  #checkFailure = $state('');
  #restoreRefusal = $state('');
  /** The saved list the store last reported, as JSON: shown while idle, or in a write's answer. */
  readonly #known: Record<RosterList, string>;
  /** Whether the list has a write in flight. */
  readonly #flight: Record<RosterList, boolean> = { profiles: false, notes: false };
  /** The newest write of the list waiting for the one in flight. */
  readonly #next: { profiles: Sent<AgentProfile> | null; notes: Sent<AgentNote> | null } = {
    profiles: null,
    notes: null,
  };
  /** Advanced by every save and every replacement of the list's draft. */
  readonly #revision: Record<RosterList, number> = { profiles: 0, notes: 0 };
  /** The revision of the save waiting on its check; 0 for none. */
  readonly #checking: Record<RosterList, number> = { profiles: 0, notes: 0 };
  #checkTicket = 0;
  #restore: symbol | null = null;

  constructor(deps: RosterDeps) {
    this.#deps = deps;
    const saved = deps.saved();
    this.#profiles = draftOf(saved.agentProfiles);
    this.#notes = draftOf(saved.agentNotes);
    this.#known = { profiles: JSON.stringify(saved.agentProfiles), notes: JSON.stringify(saved.agentNotes) };
  }

  get profiles(): ReadonlyDraft<AgentProfile> {
    return this.#profiles;
  }
  get notes(): ReadonlyDraft<AgentNote> {
    return this.#notes;
  }
  get checks(): Readonly<Record<string, Readonly<ProfileCheck>>> {
    return this.#checks;
  }
  /** Why the last check could not tell; while it stands, a save writes nothing. */
  get checkFailure(): string {
    return this.#checkFailure;
  }
  get restoreRefusal(): string {
    return this.#restoreRefusal;
  }

  /** Checks the saved profiles when the page opens. */
  mount(): void {
    void this.#check(this.#deps.saved().agentProfiles.map(trimProfile));
  }

  /** The page closed: a Restore still waiting does nothing. Saves finish. */
  close(): void {
    this.#restore = null;
  }

  /** The store shows `saved` for `list`. */
  follow(list: RosterList, saved: readonly AgentProfile[] | readonly AgentNote[]): void {
    // While a write of the list is in flight the store shows that write; its
    // answer says how it ended (#settled).
    if (this.#flight[list]) return;
    this.#learn(list, saved);
  }

  /** The computer's saved `list` is `saved`; a list the page did not know changed elsewhere. */
  #learn(list: RosterList, saved: readonly AgentProfile[] | readonly AgentNote[]): void {
    const key = JSON.stringify(saved);
    if (key === this.#known[list]) return;
    this.#known[list] = key;
    this.#changed(list, saved);
  }

  /** The saved `list` changed elsewhere to `saved`. */
  #changed(list: RosterList, saved: readonly AgentProfile[] | readonly AgentNote[]): void {
    if (this.#restore) {
      this.#restore = null;
      this.#restoreRefusal = RESTORE_OVERTAKEN;
    }
    const dropped = this.#replace(list);
    if (list === 'profiles') {
      const profiles = saved as readonly AgentProfile[];
      this.#profiles = reconcile($state.snapshot(this.#profiles) as Draft<AgentProfile>, profiles, trimProfile, sameProfile);
      void this.#check(profiles.map(trimProfile));
    } else {
      this.#notes = reconcile($state.snapshot(this.#notes) as Draft<AgentNote>, saved as readonly AgentNote[], trimNote);
    }
    // The save the change dropped writes the reconciled draft instead.
    if (dropped) void this.#save(list);
  }

  /** Typing in a profile's field. */
  editProfile(index: number, patch: Partial<AgentProfile>): void {
    this.#retire();
    const entry = this.#profiles.entries[index];
    if (entry) Object.assign(entry.value, patch);
  }

  /** Typing in a note's field. */
  editNote(index: number, patch: Partial<AgentNote>): void {
    this.#retire();
    const entry = this.#notes.entries[index];
    if (entry) Object.assign(entry.value, patch);
  }

  /** A field of `list` committed: save the list. */
  commit(list: RosterList): void {
    this.#retire();
    void this.#save(list);
  }

  addProfile(): void {
    this.#retire();
    this.#profiles.entries.push({
      value: { name: '', useFor: '', provider: '', model: '', effort: '', runtimeMode: '', instructions: '' },
      origin: null,
    });
  }

  addNote(): void {
    this.#retire();
    this.#notes.entries.push({ value: { text: '', when: '' }, origin: null });
  }

  remove(list: RosterList, index: number): void {
    this.#retire();
    remove(this.#draft(list) as Draft<object>, index);
    void this.#save(list);
  }

  /** Save mine: the person's version stays and the list saves. */
  keepMine(list: RosterList, index: number): void {
    this.#retire();
    keepMine(this.#draft(list) as Draft<object>, index);
    void this.#save(list);
  }

  /** The other version: it replaces the person's, dropping a save still checking. */
  takeTheirs(list: RosterList, index: number): void {
    this.#retire();
    this.#replace(list);
    const draft = this.#draft(list) as Draft<object>;
    takeTheirs(draft, index);
    if (unsaved(draft, list === 'profiles' ? (trimProfile as (value: object) => object) : (trimNote as (value: object) => object))) {
      void this.#save(list);
    }
  }

  /**
   * Restore writes back the roster the computer itself starts with, which
   * only it knows; one that cannot say keeps its roster and says why.
   */
  async restore(): Promise<void> {
    const gesture = Symbol('restore');
    this.#restore = gesture;
    this.#restoreRefusal = '';
    let seeds: Roster | null;
    try {
      seeds = await this.#deps.seeds();
    } catch (err) {
      if (this.#restore !== gesture) return;
      this.#restore = null;
      this.#deps.seedsFailed(err);
      return;
    }
    if (this.#restore !== gesture) return;
    this.#restore = null;
    if (seeds === null) {
      this.#restoreRefusal = this.#deps.noSeeds();
      return;
    }
    this.#replace('profiles');
    this.#replace('notes');
    // The starting roster as an unsaved edit of the saved one: every entry
    // the page holds, deleted, and every seed added, saved once the write
    // settles with them. The entries themselves are the deletions, so one
    // a write in flight stores is deleted as what it stored.
    this.#profiles = {
      entries: seeds.profiles.map((profile) => ({ value: { ...profile }, origin: null })),
      removed: [...this.#profiles.entries, ...this.#profiles.removed],
    };
    this.#notes = {
      entries: seeds.notes.map((note) => ({ value: { ...note }, origin: null })),
      removed: [...this.#notes.entries, ...this.#notes.removed],
    };
    const profiles = this.#taken(this.#profiles, 'profiles', (profile) => ({ ...profile }));
    const notes = this.#taken(this.#notes, 'notes', (note) => ({ ...note }));
    this.#request([profiles, notes]);
    void this.#check(profiles.sent.map(trimProfile));
  }

  #draft(list: RosterList): Draft<AgentProfile> | Draft<AgentNote> {
    return list === 'profiles' ? this.#profiles : this.#notes;
  }

  /** The person did something newer: a Restore waiting does nothing. */
  #retire(): void {
    this.#restore = null;
  }

  /**
   * Records a replacement of the list's draft; true when it dropped a save
   * still checking or waiting to be sent. A Restore write that loses this
   * list's part sends the other part alone.
   */
  #replace(list: RosterList): boolean {
    const waiting = this.#next[list];
    const dropped = this.#checking[list] !== 0 || waiting !== null;
    this.#revision[list] += 1;
    this.#checking[list] = 0;
    this.#next[list] = null;
    const other = this.#next[list === 'profiles' ? 'notes' : 'profiles'];
    if (waiting?.pair && other) other.pair = false;
    return dropped;
  }

  /** What a write of the draft as it is now takes and sends. */
  #taken<T extends object>(draft: Draft<T>, list: RosterList, prepare: (value: T) => T): Sent<T> {
    const entries = [...draft.entries];
    return {
      list,
      entries,
      removed: [...draft.removed],
      sent: entries.map((entry) => prepare($state.snapshot(entry.value) as T)),
    };
  }

  // A list with an entry marked as changed elsewhere too, or an entry the
  // backend would refuse, is not saved: the save waits for the person.
  async #save(list: RosterList): Promise<void> {
    if (list === 'notes') {
      if (noteProblems(this.#notes.entries.map((entry) => entry.value)).size > 0 || marked(this.#notes)) return;
      this.#revision.notes += 1;
      this.#request([this.#taken(this.#notes, 'notes', trimNote)]);
      return;
    }
    if (profileProblems(this.#profiles.entries.map((entry) => entry.value)).size > 0 || marked(this.#profiles)) return;
    const revision = ++this.#revision.profiles;
    this.#checking.profiles = revision;
    const profiles = this.#taken(this.#profiles, 'profiles', trimProfile);
    const runnable = await this.#check(profiles.sent);
    if (this.#revision.profiles !== revision) return;
    this.#checking.profiles = 0;
    if (!runnable) return;
    this.#request([profiles]);
  }

  /** Wants `parts` written: each replaces its list's waiting write, and is sent once its list is idle. */
  #request(parts: AnySent[]): void {
    for (const part of parts) {
      const replaced = this.#next[part.list];
      const other = this.#next[part.list === 'profiles' ? 'notes' : 'profiles'];
      if (replaced?.pair && other) other.pair = false;
      const waiting = { ...part, pair: parts.length > 1 };
      if (waiting.list === 'profiles') this.#next.profiles = waiting as Sent<AgentProfile>;
      else this.#next.notes = waiting as Sent<AgentNote>;
    }
    this.#dispatch();
  }

  /** Sends each waiting write whose lists have no write in flight. */
  #dispatch(): void {
    const { profiles, notes } = this.#next;
    if (profiles?.pair && notes?.pair) {
      if (this.#flight.profiles || this.#flight.notes) return;
      this.#next.profiles = null;
      this.#next.notes = null;
      this.#write([profiles, notes]);
      return;
    }
    for (const list of ['profiles', 'notes'] as const) {
      const waiting = this.#next[list];
      if (!waiting || this.#flight[list]) continue;
      this.#next[list] = null;
      this.#write([waiting]);
    }
  }

  #write(parts: AnySent[]): void {
    const patch: RosterPatch = {};
    for (const part of parts) {
      this.#flight[part.list] = true;
      if (part.list === 'profiles') patch.agentProfiles = part.sent as AgentProfile[];
      else patch.agentNotes = part.sent as AgentNote[];
    }
    void this.#deps.write(patch).then((result) => this.#settled(parts, result));
  }

  /** A write ended as `result`. */
  #settled(parts: AnySent[], result: RosterWrite): void {
    for (const part of parts) this.#flight[part.list] = false;
    if (result.status === 'local') {
      throw new Error('The computer stores no agent roster, so the roster was not saved.');
    }
    for (const part of parts) {
      if (result.status === 'dropped') {
        // How the write ended is unknown; the edits stay unsaved and the page
        // follows whatever the store shows now.
        const shown = this.#deps.saved();
        this.follow(part.list, part.list === 'profiles' ? shown.agentProfiles : shown.agentNotes);
        continue;
      }
      const holds = part.list === 'profiles' ? result.saved.agentProfiles : result.saved.agentNotes;
      if (result.status === 'saved') {
        markSaved(this.#draft(part.list) as Draft<object>, part.entries, part.sent, part.removed);
        // The computer stored this write; a list other than the one sent is
        // a real difference and is reconciled.
        if (sameList(holds as readonly object[], part.sent)) {
          this.#known[part.list] = JSON.stringify(holds);
          continue;
        }
      }
      this.#learn(part.list, holds);
    }
    this.#dispatch();
  }

  /**
   * Checks `list`; true when every profile can run, false when one cannot
   * or the check could not tell. Only the newest check's answer shows.
   */
  async #check(list: AgentProfile[]): Promise<boolean> {
    const ticket = ++this.#checkTicket;
    let results: ProfileCheck[];
    try {
      results = await this.#deps.check(list);
    } catch (err) {
      if (ticket === this.#checkTicket) this.#checkFailure = this.#deps.checkFailure(err);
      return false;
    }
    if (ticket === this.#checkTicket) {
      this.#checkFailure = '';
      const next = { ...this.#checks };
      list.forEach((profile, index) => {
        next[checkKey(profile)] = results[index] ?? {};
      });
      this.#checks = next;
    }
    return results.every((result) => !result?.problem);
  }
}
