import { describe, expect, it } from 'vitest';
import { trimNote, trimProfile } from './agentRoster';
import { dirty, type DraftEntry } from './rosterDraft';
import {
  RESTORE_OVERTAKEN,
  RosterEditor,
  type ProfileCheck,
  type Roster,
  type RosterPatch,
  type RosterWrite,
  type RosterSaved,
} from './rosterEditor.svelte';
import type { AgentNote, AgentProfile } from '../../types/settings';

const profile = (name: string, extra: Partial<AgentProfile> = {}): AgentProfile => ({
  name,
  useFor: `${name} work.`,
  provider: 'codex',
  model: 'gpt-sol',
  effort: '',
  runtimeMode: '',
  instructions: '',
  ...extra,
});
const note = (text: string): AgentNote => ({ text, when: '' });

const ALPHA = profile('Alpha');
const BETA = profile('Beta');
const SEEDS: Roster = { profiles: [profile('Seed')], notes: [note('seeded')] };

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(err: unknown): void;
}
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// Ten task turns: setImmediate, not setTimeout(0), which waits at least
// 1 ms per turn and made flush cost ~12 ms while nothing ran.
async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await new Promise((resolve) => setImmediate(resolve));
}

/**
 * The settings store as the page sees it: a write shows its patch at once,
 * settles in the order it was sent and says how it ended; a refused write
 * puts back the saved list. Every change it shows is passed to the editor,
 * as the page's effects do.
 */
class Store {
  saved: RosterSaved;
  pending: RosterPatch[] = [];
  writes: RosterPatch[] = [];
  refuse = false;
  editor: RosterEditor | null = null;

  constructor(saved: RosterSaved) {
    this.saved = saved;
  }

  shown(): RosterSaved {
    const value = { ...this.saved };
    for (const patch of this.pending) Object.assign(value, patch);
    return value;
  }

  notify(): void {
    const shown = this.shown();
    this.editor?.follow('profiles', shown.agentProfiles);
    this.editor?.follow('notes', shown.agentNotes);
  }

  /** When set, a write waits for settleNext() instead of settling at once. */
  manual = false;
  /** When set, the next write waits for settleNext() and is then refused. */
  holdRefusal = false;
  /** When set, the page hears what a settled write left only after the write resolves. */
  followLate = false;
  /** When set, a write that settles is dropped: the computer went away before it answered. */
  drop = false;
  /** When set, what the computer stores for a write it saves. */
  storedAs: ((patch: RosterPatch) => RosterPatch) | null = null;
  /** Writes not yet settled, in the order the computer settles them: a held one holds those after it. */
  #queue: { settle: () => void; held: boolean }[] = [];

  write(patch: RosterPatch): Promise<RosterWrite> {
    this.writes.push(patch);
    this.pending.push(patch);
    const refused = this.holdRefusal;
    this.holdRefusal = false;
    this.notify();
    return new Promise((resolve) => {
      const settle = () => {
        this.pending = this.pending.filter((p) => p !== patch);
        const saved = !this.refuse && !refused && !this.drop;
        if (saved) this.saved = { ...this.saved, ...(this.storedAs ? this.storedAs(patch) : patch) };
        const result: RosterWrite = this.drop
          ? { status: 'dropped' }
          : { status: saved ? 'saved' : 'refused', saved: this.saved };
        if (this.followLate) {
          resolve(result);
          setImmediate(() => this.notify());
          return;
        }
        this.notify();
        resolve(result);
      };
      this.#queue.push({ settle, held: this.manual || refused });
      queueMicrotask(() => this.#drain());
    });
  }

  #drain(): void {
    while (this.#queue[0] && !this.#queue[0].held) this.#queue.shift()!.settle();
  }

  /** Settles the oldest write still waiting: saved, or put back when it is refused. */
  settleNext(): void {
    const head = this.#queue[0];
    if (!head) return;
    head.held = false;
    this.#drain();
  }

  elsewhere(patch: RosterPatch): void {
    this.saved = { ...this.saved, ...patch };
    this.notify();
  }
}

/** An editor over `store`, whose checks and starting roster the test holds when asked. */
function harness(saved: RosterSaved) {
  const store = new Store(saved);
  const held = { checks: [] as Deferred<ProfileCheck[]>[], seeds: [] as Deferred<Roster | null>[], holdCheck: false };
  const failures: unknown[] = [];
  const editor = new RosterEditor({
    saved: () => store.shown(),
    check: (profiles) => {
      if (!held.holdCheck) return Promise.resolve(profiles.map(() => ({})));
      held.holdCheck = false;
      const check = deferred<ProfileCheck[]>();
      held.checks.push(check);
      return check.promise;
    },
    checkFailure: (err) => `could not check: ${(err as Error).message}`,
    seeds: () => {
      const seeds = deferred<Roster | null>();
      held.seeds.push(seeds);
      return seeds.promise;
    },
    noSeeds: () => 'no starting roster',
    seedsFailed: (err) => failures.push(err),
    write: (patch) => store.write(patch),
  });
  store.editor = editor;
  return { store, held, editor, failures };
}

/** Holds the check the next action starts; the returned function answers it. */
function holdingCheck(h: ReturnType<typeof harness>, act: () => void, answer: (d: Deferred<ProfileCheck[]>) => void) {
  h.held.holdCheck = true;
  const before = h.held.checks.length;
  act();
  h.held.holdCheck = false;
  const check = h.held.checks.length > before ? h.held.checks.at(-1)! : null;
  return () => {
    if (check) answer(check);
  };
}

const ok = (d: Deferred<ProfileCheck[]>) => d.resolve([{}, {}, {}, {}]);
const offline = (d: Deferred<ProfileCheck[]>) => d.reject(new Error('offline'));

const markedNote = (editor: RosterEditor) => editor.notes.entries.findIndex((entry) => entry.theirs !== undefined);

type Event =
  | 'mount'
  | 'save'
  | 'refused'
  | 'restore'
  | 'elsewhere'
  | 'typing'
  | 'add'
  | 'delete'
  | 'keepMine'
  | 'takeTheirs'
  | 'close';
const EVENTS: Event[] = ['mount', 'save', 'refused', 'restore', 'elsewhere', 'typing', 'add', 'delete', 'keepMine', 'takeTheirs', 'close'];
const ASYNC = new Set<Event>(['mount', 'save', 'refused', 'restore', 'delete']);

/** Starts `event`; the returned function completes what it waits on. */
function start(h: ReturnType<typeof harness>, event: Event): () => void {
  const { editor, store, held } = h;
  switch (event) {
    case 'mount':
      return holdingCheck(h, () => editor.mount(), offline);
    case 'save':
      return holdingCheck(h, () => {
        editor.editProfile(0, { name: 'Sam' });
        editor.commit('profiles');
      }, ok);
    case 'refused':
      // A save whose write the computer holds, then refuses.
      store.holdRefusal = true;
      editor.editProfile(1, { name: 'Ref' });
      editor.commit('profiles');
      return () => store.settleNext();
    case 'restore': {
      void editor.restore();
      const seeds = held.seeds.at(-1)!;
      return () => seeds.resolve(SEEDS);
    }
    case 'elsewhere':
      store.elsewhere({ agentProfiles: [profile('Alpha', { model: 'gpt-terra' }), BETA] });
      return () => {};
    case 'typing':
      editor.editProfile(0, { useFor: 'Typed.' });
      return () => {};
    case 'add':
      editor.addProfile();
      return () => {};
    case 'delete':
      return holdingCheck(h, () => editor.remove('profiles', editor.profiles.entries.length - 1), ok);
    case 'keepMine': {
      const index = markedNote(editor);
      if (index >= 0) editor.keepMine('notes', index);
      return () => {};
    }
    case 'takeTheirs': {
      const index = markedNote(editor);
      if (index >= 0) editor.takeTheirs('notes', index);
      return () => {};
    }
    case 'close':
      editor.close();
      return () => {};
  }
}

const names = (list: readonly { name: string }[]) => list.map((p) => p.name).join(',');
const texts = (list: readonly { text: string }[]) => list.map((n) => n.text).join(',');

/**
 * One line for what a row ends with: the writes in order, then each list as
 * the draft holds it (an unsaved entry ends in *, a marked one in !), then
 * what the Restore slot and the check say.
 */
function outcome(h: ReturnType<typeof harness>): string {
  const { editor, store } = h;
  const writes = store.writes
    .map((w) => [w.agentProfiles && `P:${names(w.agentProfiles)}`, w.agentNotes && `N:${texts(w.agentNotes)}`].filter(Boolean).join('+'))
    .join(' ');
  const mark = <T extends object>(entry: DraftEntry<T>, trim: (value: T) => T, label: string) =>
    label + (entry.theirs !== undefined ? '!' : dirty(entry, trim) ? '*' : '');
  const profiles = editor.profiles.entries
    .map((e) => mark(e as DraftEntry<AgentProfile>, trimProfile, `${e.value.name || '_'}${e.value.useFor === 'Typed.' ? '~' : ''}`))
    .join(',');
  const notes = editor.notes.entries.map((e) => mark(e as DraftEntry<AgentNote>, trimNote, e.value.text)).join(',');
  const deleting = editor.profiles.removed.filter((gone) => gone.origin !== null).map((gone) => gone.origin!);
  const removed = deleting.length ? ` -${names(deleting)}` : '';
  const restore = editor.restoreRefusal === RESTORE_OVERTAKEN ? ' overtaken' : editor.restoreRefusal ? ` refused` : '';
  const check = editor.checkFailure ? ' checkFailed' : '';
  return `[${writes}] ${profiles}${removed} | ${notes}${restore}${check}`;
}

/**
 * Saved: profiles Alpha and Beta, a note "one". The note was then edited
 * here to "mine" while elsewhere changed it to "theirs", so the notes draft
 * holds "theirs" and "mine", marked.
 */
async function baseline() {
  const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [note('one')] });
  h.editor.editNote(0, { text: 'mine' });
  h.store.elsewhere({ agentNotes: [note('theirs')] });
  await flush();
  return h;
}

async function run(first: Event, second: Event, order: 'first' | 'second'): Promise<string> {
  const h = await baseline();
  const completeFirst = start(h, first);
  await flush();
  const completeSecond = start(h, second);
  await flush();
  for (const complete of order === 'first' ? [completeFirst, completeSecond] : [completeSecond, completeFirst]) {
    complete();
    await flush();
  }
  return outcome(h);
}

// Every ordered pair of the page's events: the first starts and waits on
// what it waits for (a check, the starting roster), the second starts, and
// then each completes, the first's wait answering first or second. An event
// with nothing to wait on completes as it starts, so a pair of two such
// events has one order. The baseline and each event are described on
// baseline() and start(); the line a row ends with on outcome().
//
// The mount check here always fails (" checkFailed"), so a row shows
// whether its failure is the one on the page; every other check passes.
const TABLE: Record<string, string> = {
  "mount > mount, earlier answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "mount > mount, later answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "mount > save, mount answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "mount > save, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "mount > refused, mount answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "mount > refused, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "mount > restore, mount answers first": "[P:Seed+N:seeded] Seed | seeded",
  "mount > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "mount > elsewhere, mount answers first": "[] Alpha,Beta | theirs,mine!",
  "mount > elsewhere, elsewhere answers first": "[] Alpha,Beta | theirs,mine!",
  "mount > typing, mount answers first": "[] Alpha~*,Beta | theirs,mine! checkFailed",
  "mount > typing, typing answers first": "[] Alpha~*,Beta | theirs,mine! checkFailed",
  "mount > add, mount answers first": "[] Alpha,Beta,_* | theirs,mine! checkFailed",
  "mount > add, add answers first": "[] Alpha,Beta,_* | theirs,mine! checkFailed",
  "mount > delete, mount answers first": "[P:Alpha] Alpha | theirs,mine!",
  "mount > delete, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "mount > keepMine, mount answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine checkFailed",
  "mount > keepMine, keepMine answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine checkFailed",
  "mount > takeTheirs, mount answers first": "[] Alpha,Beta | theirs checkFailed",
  "mount > takeTheirs, takeTheirs answers first": "[] Alpha,Beta | theirs checkFailed",
  "mount > close, mount answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "mount > close, close answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "save > mount, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine! checkFailed",
  "save > mount, mount answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine! checkFailed",
  "save > save, earlier answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "save > save, later answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "save > refused, save answers first": "[P:Sam,Ref] Sam*,Ref* | theirs,mine!",
  "save > refused, refused answers first": "[P:Sam,Ref] Sam*,Ref* | theirs,mine!",
  "save > restore, save answers first": "[P:Sam,Beta P:Seed+N:seeded] Seed | seeded",
  "save > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "save > elsewhere, save answers first": "[] Sam!,Beta | theirs,mine!",
  "save > elsewhere, elsewhere answers first": "[] Sam!,Beta | theirs,mine!",
  "save > typing, save answers first": "[P:Sam,Beta] Sam~*,Beta | theirs,mine!",
  "save > typing, typing answers first": "[P:Sam,Beta] Sam~*,Beta | theirs,mine!",
  "save > add, save answers first": "[P:Sam,Beta] Sam,Beta,_* | theirs,mine!",
  "save > add, add answers first": "[P:Sam,Beta] Sam,Beta,_* | theirs,mine!",
  "save > delete, save answers first": "[P:Sam] Sam | theirs,mine!",
  "save > delete, delete answers first": "[P:Sam] Sam | theirs,mine!",
  "save > keepMine, save answers first": "[N:theirs,mine P:Sam,Beta] Sam,Beta | theirs,mine",
  "save > keepMine, keepMine answers first": "[N:theirs,mine P:Sam,Beta] Sam,Beta | theirs,mine",
  "save > takeTheirs, save answers first": "[P:Sam,Beta] Sam,Beta | theirs",
  "save > takeTheirs, takeTheirs answers first": "[P:Sam,Beta] Sam,Beta | theirs",
  "save > close, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "save > close, close answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "refused > mount, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine! checkFailed",
  "refused > mount, mount answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine! checkFailed",
  "refused > save, refused answers first": "[P:Alpha,Ref P:Sam,Ref] Sam,Ref | theirs,mine!",
  "refused > save, save answers first": "[P:Alpha,Ref P:Sam,Ref] Sam,Ref | theirs,mine!",
  "refused > refused, earlier answers first": "[P:Alpha,Ref P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "refused > refused, later answers first": "[P:Alpha,Ref P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "refused > restore, refused answers first": "[P:Alpha,Ref P:Seed+N:seeded] Seed | seeded",
  "refused > restore, restore answers first": "[P:Alpha,Ref P:Seed+N:seeded] Seed | seeded",
  "refused > elsewhere, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "refused > elsewhere, elsewhere answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "refused > typing, refused answers first": "[P:Alpha,Ref] Alpha~*,Ref* | theirs,mine!",
  "refused > typing, typing answers first": "[P:Alpha,Ref] Alpha~*,Ref* | theirs,mine!",
  "refused > add, refused answers first": "[P:Alpha,Ref] Alpha,Ref*,_* | theirs,mine!",
  "refused > add, add answers first": "[P:Alpha,Ref] Alpha,Ref*,_* | theirs,mine!",
  "refused > delete, refused answers first": "[P:Alpha,Ref P:Alpha] Alpha | theirs,mine!",
  "refused > delete, delete answers first": "[P:Alpha,Ref P:Alpha] Alpha | theirs,mine!",
  "refused > keepMine, refused answers first": "[P:Alpha,Ref N:theirs,mine] Alpha,Ref* | theirs,mine",
  "refused > keepMine, keepMine answers first": "[P:Alpha,Ref N:theirs,mine] Alpha,Ref* | theirs,mine",
  "refused > takeTheirs, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs",
  "refused > takeTheirs, takeTheirs answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs",
  "refused > close, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "refused > close, close answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "restore > mount, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "restore > mount, mount answers first": "[P:Seed+N:seeded] Seed | seeded",
  "restore > save, restore answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "restore > save, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "restore > refused, restore answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "restore > refused, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "restore > restore, earlier answers first": "[P:Seed+N:seeded] Seed | seeded",
  "restore > restore, later answers first": "[P:Seed+N:seeded] Seed | seeded",
  "restore > elsewhere, restore answers first": "[] Alpha,Beta | theirs,mine! overtaken",
  "restore > elsewhere, elsewhere answers first": "[] Alpha,Beta | theirs,mine! overtaken",
  "restore > typing, restore answers first": "[] Alpha~*,Beta | theirs,mine!",
  "restore > typing, typing answers first": "[] Alpha~*,Beta | theirs,mine!",
  "restore > add, restore answers first": "[] Alpha,Beta,_* | theirs,mine!",
  "restore > add, add answers first": "[] Alpha,Beta,_* | theirs,mine!",
  "restore > delete, restore answers first": "[P:Alpha] Alpha | theirs,mine!",
  "restore > delete, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "restore > keepMine, restore answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "restore > keepMine, keepMine answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "restore > takeTheirs, restore answers first": "[] Alpha,Beta | theirs",
  "restore > takeTheirs, takeTheirs answers first": "[] Alpha,Beta | theirs",
  "restore > close, restore answers first": "[] Alpha,Beta | theirs,mine!",
  "restore > close, close answers first": "[] Alpha,Beta | theirs,mine!",
  "elsewhere > mount, elsewhere answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "elsewhere > mount, mount answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "elsewhere > save, elsewhere answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "elsewhere > save, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "elsewhere > refused, elsewhere answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "elsewhere > refused, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "elsewhere > restore, elsewhere answers first": "[P:Seed+N:seeded] Seed | seeded",
  "elsewhere > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "elsewhere > elsewhere": "[] Alpha,Beta | theirs,mine!",
  "elsewhere > typing": "[] Alpha~*,Beta | theirs,mine!",
  "elsewhere > add": "[] Alpha,Beta,_* | theirs,mine!",
  "elsewhere > delete, elsewhere answers first": "[P:Alpha] Alpha | theirs,mine!",
  "elsewhere > delete, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "elsewhere > keepMine": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "elsewhere > takeTheirs": "[] Alpha,Beta | theirs",
  "elsewhere > close": "[] Alpha,Beta | theirs,mine!",
  "typing > mount, typing answers first": "[] Alpha~*,Beta | theirs,mine! checkFailed",
  "typing > mount, mount answers first": "[] Alpha~*,Beta | theirs,mine! checkFailed",
  "typing > save, typing answers first": "[P:Sam,Beta] Sam~,Beta | theirs,mine!",
  "typing > save, save answers first": "[P:Sam,Beta] Sam~,Beta | theirs,mine!",
  "typing > refused, typing answers first": "[P:Alpha,Ref] Alpha~*,Ref* | theirs,mine!",
  "typing > refused, refused answers first": "[P:Alpha,Ref] Alpha~*,Ref* | theirs,mine!",
  "typing > restore, typing answers first": "[P:Seed+N:seeded] Seed | seeded",
  "typing > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "typing > elsewhere": "[] Alpha~!,Beta | theirs,mine!",
  "typing > typing": "[] Alpha~*,Beta | theirs,mine!",
  "typing > add": "[] Alpha~*,Beta,_* | theirs,mine!",
  "typing > delete, typing answers first": "[P:Alpha] Alpha~ | theirs,mine!",
  "typing > delete, delete answers first": "[P:Alpha] Alpha~ | theirs,mine!",
  "typing > keepMine": "[N:theirs,mine] Alpha~*,Beta | theirs,mine",
  "typing > takeTheirs": "[] Alpha~*,Beta | theirs",
  "typing > close": "[] Alpha~*,Beta | theirs,mine!",
  "add > mount, add answers first": "[] Alpha,Beta,_* | theirs,mine! checkFailed",
  "add > mount, mount answers first": "[] Alpha,Beta,_* | theirs,mine! checkFailed",
  "add > save, add answers first": "[] Sam*,Beta,_* | theirs,mine!",
  "add > save, save answers first": "[] Sam*,Beta,_* | theirs,mine!",
  "add > refused, add answers first": "[] Alpha,Ref*,_* | theirs,mine!",
  "add > refused, refused answers first": "[] Alpha,Ref*,_* | theirs,mine!",
  "add > restore, add answers first": "[P:Seed+N:seeded] Seed | seeded",
  "add > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "add > elsewhere": "[] Alpha,Beta,_* | theirs,mine!",
  "add > typing": "[] Alpha~*,Beta,_* | theirs,mine!",
  "add > add": "[] Alpha,Beta,_*,_* | theirs,mine!",
  "add > delete, add answers first": "[P:Alpha,Beta] Alpha,Beta | theirs,mine!",
  "add > delete, delete answers first": "[P:Alpha,Beta] Alpha,Beta | theirs,mine!",
  "add > keepMine": "[N:theirs,mine] Alpha,Beta,_* | theirs,mine",
  "add > takeTheirs": "[] Alpha,Beta,_* | theirs",
  "add > close": "[] Alpha,Beta,_* | theirs,mine!",
  "delete > mount, delete answers first": "[P:Alpha] Alpha | theirs,mine! checkFailed",
  "delete > mount, mount answers first": "[P:Alpha] Alpha | theirs,mine! checkFailed",
  "delete > save, delete answers first": "[P:Sam] Sam | theirs,mine!",
  "delete > save, save answers first": "[P:Sam] Sam | theirs,mine!",
  "delete > refused, delete answers first": "[P:Alpha] Alpha -Beta | theirs,mine!",
  "delete > refused, refused answers first": "[P:Alpha] Alpha -Beta | theirs,mine!",
  "delete > restore, delete answers first": "[P:Alpha P:Seed+N:seeded] Seed | seeded",
  "delete > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "delete > elsewhere, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "delete > elsewhere, elsewhere answers first": "[P:Alpha] Alpha | theirs,mine!",
  "delete > typing, delete answers first": "[P:Alpha] Alpha~* | theirs,mine!",
  "delete > typing, typing answers first": "[P:Alpha] Alpha~* | theirs,mine!",
  "delete > add, delete answers first": "[P:Alpha] Alpha,_* | theirs,mine!",
  "delete > add, add answers first": "[P:Alpha] Alpha,_* | theirs,mine!",
  "delete > delete, earlier answers first": "[P:]  | theirs,mine!",
  "delete > delete, later answers first": "[P:]  | theirs,mine!",
  "delete > keepMine, delete answers first": "[N:theirs,mine P:Alpha] Alpha | theirs,mine",
  "delete > keepMine, keepMine answers first": "[N:theirs,mine P:Alpha] Alpha | theirs,mine",
  "delete > takeTheirs, delete answers first": "[P:Alpha] Alpha | theirs",
  "delete > takeTheirs, takeTheirs answers first": "[P:Alpha] Alpha | theirs",
  "delete > close, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "delete > close, close answers first": "[P:Alpha] Alpha | theirs,mine!",
  "keepMine > mount, keepMine answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine checkFailed",
  "keepMine > mount, mount answers first": "[N:theirs,mine] Alpha,Beta | theirs,mine checkFailed",
  "keepMine > save, keepMine answers first": "[N:theirs,mine P:Sam,Beta] Sam,Beta | theirs,mine",
  "keepMine > save, save answers first": "[N:theirs,mine P:Sam,Beta] Sam,Beta | theirs,mine",
  "keepMine > refused, keepMine answers first": "[N:theirs,mine P:Alpha,Ref] Alpha,Ref* | theirs,mine",
  "keepMine > refused, refused answers first": "[N:theirs,mine P:Alpha,Ref] Alpha,Ref* | theirs,mine",
  "keepMine > restore, keepMine answers first": "[N:theirs,mine P:Seed+N:seeded] Seed | seeded",
  "keepMine > restore, restore answers first": "[N:theirs,mine P:Seed+N:seeded] Seed | seeded",
  "keepMine > elsewhere": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "keepMine > typing": "[N:theirs,mine] Alpha~*,Beta | theirs,mine",
  "keepMine > add": "[N:theirs,mine] Alpha,Beta,_* | theirs,mine",
  "keepMine > delete, keepMine answers first": "[N:theirs,mine P:Alpha] Alpha | theirs,mine",
  "keepMine > delete, delete answers first": "[N:theirs,mine P:Alpha] Alpha | theirs,mine",
  "keepMine > keepMine": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "keepMine > takeTheirs": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "keepMine > close": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "takeTheirs > mount, takeTheirs answers first": "[] Alpha,Beta | theirs checkFailed",
  "takeTheirs > mount, mount answers first": "[] Alpha,Beta | theirs checkFailed",
  "takeTheirs > save, takeTheirs answers first": "[P:Sam,Beta] Sam,Beta | theirs",
  "takeTheirs > save, save answers first": "[P:Sam,Beta] Sam,Beta | theirs",
  "takeTheirs > refused, takeTheirs answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs",
  "takeTheirs > refused, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs",
  "takeTheirs > restore, takeTheirs answers first": "[P:Seed+N:seeded] Seed | seeded",
  "takeTheirs > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "takeTheirs > elsewhere": "[] Alpha,Beta | theirs",
  "takeTheirs > typing": "[] Alpha~*,Beta | theirs",
  "takeTheirs > add": "[] Alpha,Beta,_* | theirs",
  "takeTheirs > delete, takeTheirs answers first": "[P:Alpha] Alpha | theirs",
  "takeTheirs > delete, delete answers first": "[P:Alpha] Alpha | theirs",
  "takeTheirs > keepMine": "[] Alpha,Beta | theirs",
  "takeTheirs > takeTheirs": "[] Alpha,Beta | theirs",
  "takeTheirs > close": "[] Alpha,Beta | theirs",
  "close > mount, close answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "close > mount, mount answers first": "[] Alpha,Beta | theirs,mine! checkFailed",
  "close > save, close answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "close > save, save answers first": "[P:Sam,Beta] Sam,Beta | theirs,mine!",
  "close > refused, close answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "close > refused, refused answers first": "[P:Alpha,Ref] Alpha,Ref* | theirs,mine!",
  "close > restore, close answers first": "[P:Seed+N:seeded] Seed | seeded",
  "close > restore, restore answers first": "[P:Seed+N:seeded] Seed | seeded",
  "close > elsewhere": "[] Alpha,Beta | theirs,mine!",
  "close > typing": "[] Alpha~*,Beta | theirs,mine!",
  "close > add": "[] Alpha,Beta,_* | theirs,mine!",
  "close > delete, close answers first": "[P:Alpha] Alpha | theirs,mine!",
  "close > delete, delete answers first": "[P:Alpha] Alpha | theirs,mine!",
  "close > keepMine": "[N:theirs,mine] Alpha,Beta | theirs,mine",
  "close > takeTheirs": "[] Alpha,Beta | theirs",
  "close > close": "[] Alpha,Beta | theirs,mine!",
};

describe('the roster editor orders every pair of events', () => {
  const rows: [string, Event, Event, 'first' | 'second'][] = [];
  for (const first of EVENTS) {
    for (const second of EVENTS) {
      const orders: ('first' | 'second')[] = ASYNC.has(first) || ASYNC.has(second) ? ['first', 'second'] : ['first'];
      for (const order of orders) {
        const answersFirst = first === second ? (order === 'first' ? 'earlier' : 'later') : order === 'first' ? first : second;
        rows.push([`${first} > ${second}${orders.length > 1 ? `, ${answersFirst} answers first` : ''}`, first, second, order]);
      }
    }
  }
  it.each(rows)('%s', async (name, first, second, order) => {
    const got = await run(first, second, order);
    if (process.env.ROSTER_TABLE_PRINT) console.log(`  ${JSON.stringify(name)}: ${JSON.stringify(got)},`);
    expect(got).toBe(TABLE[name]);
  });
});

describe('a write the computer refuses', () => {
  // The store puts back the saved list with its error toast; the person's
  // edits stay on the page, unsaved, and the next commit sends them again.
  it('leaves the typed values in the draft, unsaved, for the next commit to send', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.refuse = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    expect(h.store.writes).toHaveLength(1);
    expect(outcome(h)).toBe('[P:Sam,Beta] Sam*,Beta | ');

    h.store.refuse = false;
    h.editor.commit('profiles');
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Sam,Beta] Sam,Beta | ');
  });

  // Restore writes through the same path: refused, the starting roster stays
  // on the page as an unsaved edit of the saved one.
  it('leaves a refused Restore on the page, unsaved, for the next commit to send', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [note('one')] });
    h.store.refuse = true;
    void h.editor.restore();
    h.held.seeds[0].resolve(SEEDS);
    await flush();
    expect(outcome(h)).toBe('[P:Seed+N:seeded] Seed* -Alpha,Beta | seeded*');

    h.store.refuse = false;
    h.editor.commit('profiles');
    await flush();
    expect(outcome(h)).toBe('[P:Seed+N:seeded P:Seed] Seed | seeded*');
  });

  // The page's effect hears of the put-back list before the write resolves
  // today; the draft must not depend on that order.
  it('leaves the edits unsaved when the write resolves before the page hears it was put back', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.refuse = true;
    h.store.followLate = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta] Sam*,Beta | ');
  });

  // The store putting back this page's own refused write is no change made
  // elsewhere: a Restore waiting across it still acts on its answer.
  it('leaves a Restore waiting across a refused write to act on its answer', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [note('one')] });
    h.store.holdRefusal = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    void h.editor.restore();
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta] Sam*,Beta | one');

    h.held.seeds[0].resolve(SEEDS);
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Seed+N:seeded] Seed | seeded');
  });

  // While a later write is outstanding the store shows its list, so an
  // earlier write's settling says nothing of how it ended.
  it('keeps the edits of two writes of one list that are both refused', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.manual = true;
    h.store.refuse = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    h.editor.commit('profiles');
    await flush();
    h.store.settleNext();
    await flush();
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Sam,Beta] Sam*,Beta | ');
  });

  it('marks saved what an earlier write saved when a later one is refused', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.manual = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    h.editor.editProfile(0, { name: 'Sid' });
    h.editor.commit('profiles');
    await flush();
    h.store.settleNext();
    await flush();
    h.store.refuse = true;
    h.store.settleNext();
    await flush();
    // Sid is unsaved over the saved Sam, not marked as changed elsewhere too.
    expect(outcome(h)).toBe('[P:Sam,Beta P:Sid,Beta] Sid*,Beta | ');
    expect(h.editor.profiles.entries[0].origin?.name).toBe('Sam');
  });

  // A refusal changes no saved list, so it drops nothing: a save that was
  // checking writes once its check answers, and nothing re-saves the refused
  // write on its own.
  it('lets a save that was checking write once, and re-saves nothing', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.manual = true;
    h.store.refuse = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    h.editor.editProfile(0, { useFor: 'Typed.' });
    const answer = holdingCheck(h, () => h.editor.commit('profiles'), ok);
    h.store.settleNext();
    await flush();
    expect(h.store.writes).toHaveLength(1);
    answer();
    await flush();
    expect(h.store.writes).toHaveLength(2);
    h.store.settleNext();
    await flush();

    expect(h.store.writes).toHaveLength(2);
    expect(outcome(h)).toBe('[P:Sam,Beta P:Sam,Beta] Sam~*,Beta | ');
  });

  // The computer went away before it answered: how the write ended is
  // unknown, so nothing is marked saved and the page follows what the store
  // shows.
  // The computer stored something other than what was sent: its answer is
  // what is saved, and the page shows it.
  it('shows what the computer stored when it differs from what was sent', async () => {
    const h = harness({ agentProfiles: [], agentNotes: [note('one')] });
    h.store.storedAs = () => ({ agentNotes: [note('stored')] });
    h.editor.editNote(0, { text: 'mine' });
    h.editor.commit('notes');
    await flush();
    expect(outcome(h)).toBe('[N:mine]  | stored');
  });

  it('leaves the edits of a dropped write unsaved', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.drop = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta] Sam*,Beta | ');
  });
});

// The store shows a write in flight as the list it sends, so the page can
// tell its own writes from a change saved elsewhere only with one write of a
// list in flight at a time.
describe('one write of a list in flight at a time', () => {
  it('sends a newer save of a list once the write in flight settles', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.manual = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    h.editor.editProfile(0, { name: 'Sid' });
    h.editor.commit('profiles');
    await flush();
    expect(h.store.writes).toHaveLength(1);
    h.store.settleNext();
    await flush();
    expect(h.store.writes).toHaveLength(2);
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Sid,Beta] Sid,Beta | ');
  });

  it('sends Restore\'s write once a write in flight settles', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [note('one')] });
    h.store.manual = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    void h.editor.restore();
    h.held.seeds[0].resolve(SEEDS);
    await flush();
    expect(h.store.writes).toHaveLength(1);
    h.store.settleNext();
    await flush();
    expect(h.store.writes).toHaveLength(2);
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Seed+N:seeded] Seed | seeded');
  });
});

// The reviewer's sequence: a rename's write is in flight when the row is
// deleted; the rename is saved and the deletion refused. The deletion
// follows the entry, so it now deletes the renamed row, still unsaved.
describe('a deletion made while an edit of the same row is in flight', () => {
  it('survives the edit being saved and the deletion refused, for profiles', async () => {
    const h = harness({ agentProfiles: [ALPHA, BETA], agentNotes: [] });
    h.store.manual = true;
    h.editor.editProfile(0, { name: 'Sam' });
    h.editor.commit('profiles');
    await flush();
    h.editor.remove('profiles', 0);
    await flush();
    h.store.settleNext();
    await flush();
    h.store.refuse = true;
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Beta] Beta -Sam | ');

    h.store.refuse = false;
    h.store.manual = false;
    h.editor.commit('profiles');
    await flush();
    expect(outcome(h)).toBe('[P:Sam,Beta P:Beta P:Beta] Beta | ');
  });

  it('survives the edit being saved and the deletion refused, for notes', async () => {
    const h = harness({ agentProfiles: [], agentNotes: [note('one'), note('two')] });
    h.store.manual = true;
    h.editor.editNote(0, { text: 'renamed' });
    h.editor.commit('notes');
    await flush();
    h.editor.remove('notes', 0);
    await flush();
    h.store.settleNext();
    await flush();
    h.store.refuse = true;
    h.store.settleNext();
    await flush();
    expect(outcome(h)).toBe('[N:renamed,two N:two]  | two');
    expect(h.editor.notes.removed.map((gone) => gone.origin?.text)).toEqual(['renamed']);

    h.store.refuse = false;
    h.store.manual = false;
    h.editor.commit('notes');
    await flush();
    expect(h.store.saved.agentNotes.map((n) => n.text)).toEqual(['two']);
    expect(h.editor.notes.removed).toEqual([]);
  });

  it('deletes a row added and deleted before its first write settles', async () => {
    const h = harness({ agentProfiles: [ALPHA], agentNotes: [] });
    h.store.manual = true;
    h.editor.addProfile();
    h.editor.editProfile(1, profile('Added'));
    h.editor.commit('profiles');
    await flush();
    h.editor.remove('profiles', 1);
    await flush();
    h.store.settleNext();
    await flush();
    h.store.settleNext();
    await flush();
    expect(h.store.saved.agentProfiles.map((p) => p.name)).toEqual(['Alpha']);
    expect(outcome(h)).toBe('[P:Alpha,Added P:Alpha] Alpha | ');
  });
});
