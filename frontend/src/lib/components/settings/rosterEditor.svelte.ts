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
// - The page follows the saved list as it last knew it: what the store
//   showed, or what this page sent, set before the write.
// - While a write of this page is outstanding the store shows the list this
//   page sent last, so the page learns how its writes of a list ended only
//   when the last of them settles, and decides it then (#land): the newest
//   write whose list the store holds was saved and marks its entries saved;
//   the writes after it were refused and leave their edits on the page,
//   unsaved, for the next commit to send again. A list that is neither one
//   this page sent nor the one its first write replaced changed elsewhere.
import { noteProblems, profileProblems, trimNote, trimProfile } from './agentRoster';
import {
  draftOf,
  keepMine,
  markSaved,
  marked,
  reconcile,
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
  /** Writes through the store, which shows the patch at once. */
  write(patch: RosterPatch): Promise<void>;
}

export type ReadonlyDraft<T> = {
  readonly entries: readonly {
    readonly value: Readonly<T>;
    readonly origin: Readonly<T> | null;
    readonly theirs?: Readonly<T> | null;
  }[];
  readonly removed: readonly Readonly<T>[];
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

/** One list's part of a write: what the save took, and what it sent. */
interface Sent<T> {
  list: RosterList;
  entries: DraftEntry<T>[];
  removed: T[];
  sent: T[];
}

/**
 * One list's part of a write of this page whose outcome the page has not
 * decided: what it sent, the list as the page knew it before (as JSON), and
 * whether the write's promise settled.
 */
interface Outstanding<T> extends Sent<T> {
  replaced: string;
  settled: boolean;
}

export class RosterEditor {
  readonly #deps: RosterDeps;
  #profiles = $state<Draft<AgentProfile>>({ entries: [], removed: [] });
  #notes = $state<Draft<AgentNote>>({ entries: [], removed: [] });
  #checks = $state<Record<string, ProfileCheck>>({});
  #checkFailure = $state('');
  #restoreRefusal = $state('');
  /** The saved list as this page last knew it, as JSON. */
  readonly #known: Record<RosterList, string>;
  /** This page's writes of each list, oldest first, until the last of them settles. */
  readonly #outstanding: { profiles: Outstanding<AgentProfile>[]; notes: Outstanding<AgentNote>[] } = {
    profiles: [],
    notes: [],
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
    // What the store shows while this page's writes are outstanding is their
    // outcome, which #land decides once the last of them settles.
    if (this.#outstanding[list].length > 0) return;
    const key = JSON.stringify(saved);
    if (key === this.#known[list]) return;
    this.#known[list] = key;
    this.#changed(list, saved, false);
  }

  /** The store holds `saved` for `list`, which this page knew otherwise; `own` when this page's write put it there. */
  #changed(list: RosterList, saved: readonly AgentProfile[] | readonly AgentNote[], own: boolean): void {
    if (this.#restore && !own) {
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
    const draft = this.#draft(list) as Draft<object>;
    const [entry] = draft.entries.splice(index, 1);
    if (entry?.origin) draft.removed.push(entry.origin);
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
    const saved = this.#deps.saved();
    // The starting roster as an unsaved edit of the saved one: every saved
    // entry pending deletion and every seed added, saved once the write
    // settles with them.
    this.#profiles = {
      entries: seeds.profiles.map((profile) => ({ value: { ...profile }, origin: null })),
      removed: saved.agentProfiles.map((profile) => ({ ...profile })),
    };
    this.#notes = {
      entries: seeds.notes.map((note) => ({ value: { ...note }, origin: null })),
      removed: saved.agentNotes.map((note) => ({ ...note })),
    };
    const profiles = this.#taken(this.#profiles, 'profiles', (profile) => ({ ...profile }));
    const notes = this.#taken(this.#notes, 'notes', (note) => ({ ...note }));
    this.#send({ agentProfiles: profiles.sent, agentNotes: notes.sent }, [profiles, notes]);
    void this.#check(profiles.sent.map(trimProfile));
  }

  #draft(list: RosterList): Draft<AgentProfile> | Draft<AgentNote> {
    return list === 'profiles' ? this.#profiles : this.#notes;
  }

  /** The person did something newer: a Restore waiting does nothing. */
  #retire(): void {
    this.#restore = null;
  }

  /** Records a replacement of the list's draft; true when it dropped a save still checking. */
  #replace(list: RosterList): boolean {
    const dropped = this.#checking[list] !== 0;
    this.#revision[list] += 1;
    this.#checking[list] = 0;
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
      const notes = this.#taken(this.#notes, 'notes', trimNote);
      this.#send({ agentNotes: notes.sent }, [notes]);
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
    this.#send({ agentProfiles: profiles.sent }, [profiles]);
  }

  #send(patch: RosterPatch, parts: (Sent<AgentProfile> | Sent<AgentNote>)[]): void {
    const writes = parts.map((part) => {
      const write = { ...part, replaced: this.#known[part.list], settled: false };
      (this.#outstanding[part.list] as Outstanding<object>[]).push(write);
      this.#known[part.list] = JSON.stringify(part.sent);
      return write;
    });
    void this.#deps.write(patch).then(() => {
      for (const write of writes) {
        write.settled = true;
        if (this.#outstanding[write.list].every((other) => other.settled)) this.#land(write.list);
      }
    });
  }

  /**
   * The last of this page's outstanding writes of `list` settled, so the
   * store shows what it holds. The newest write whose list it holds was
   * saved, and the writes after it refused; a list that is none of them and
   * not the one the first write replaced changed elsewhere meanwhile.
   */
  #land(list: RosterList): void {
    const writes = this.#outstanding[list] as Outstanding<object>[];
    this.#outstanding[list] = [];
    const shown = this.#deps.saved();
    const holds = (list === 'profiles' ? shown.agentProfiles : shown.agentNotes) as readonly object[];
    const saved = [...writes].reverse().find((write) => sameList(holds, write.sent));
    if (saved) markSaved(this.#draft(list) as Draft<object>, saved.entries, saved.sent, saved.removed);
    const key = JSON.stringify(holds);
    if (key === this.#known[list]) return;
    this.#known[list] = key;
    this.#changed(list, holds as readonly AgentProfile[] | readonly AgentNote[], saved !== undefined || key === writes[0].replaced);
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
