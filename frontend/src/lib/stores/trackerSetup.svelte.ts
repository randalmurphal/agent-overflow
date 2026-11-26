// A project's tracker setup file as this window holds it (the Status
// groups page, every ticket of the project and its Start work form), and
// the one way it is changed. One entry per project, held while a surface
// holds it, keyed by the project: the project route takes each call to its
// computer.
//
// The held view is read when the first surface holds it, again on
// `tracker:setup_updated` from the project's computer and when that
// computer connects again (after the save, when a save is running), and is
// replaced by the `stored` of every save's answer. An edit is built only from a view known to be the stored one
// (`current`): one that a read or a save answered with no failure since.
// A failed read keeps the view it had; a failed first read holds none.
//
// A save (`editTrackerSetup`) applies a transform of the held setup and
// sends it with the held revision, so it carries every section and every
// site's entry it does not change, and the computer keeps the sections a
// newer build saved. Its outcome is the computer's: `saved`, or `refused`
// with the computer's sentence when the file moved since (the view is then
// the one stored); any failure (the write may or may not have happened,
// a section a newer build saved, a setup the computer refuses) rejects
// with the computer's error and reads again, and never reports saved. One
// save runs at a time for a project. A save with no surface holding the
// project holds it for the save's length.
//
// The status marks of the project's tickets read its status groups from
// the held setup (`trackerRegroupsOf`): the ticket screen holds its
// project's, the thread rows and header theirs (`threadLinkMarks`).
//
// The pages' statuses (`ProjectTrackerStatuses`) are held the same way,
// read on hold, on the event and on reconnect: the statuses the setup
// regroups off the boards, and the places its Start work defaults name,
// are part of their answer.
//
// The Fields page's fields to pin are held by site
// (`ProjectTrackerSiteFields`), each its own read, so a site that fails
// or stalls holds up no other: read on hold, on reconnect and on the
// site's Try again. A tracker read, which no change of the setup moves;
// the page holds a site again when the connections that reach it change.

import { SvelteMap } from 'svelte/reactivity';
import type { AgentRules, PinField, ProjectStatuses, Setup, SetupView, SiteStatuses, StartPlace } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { ProjectTrackerSetup, ProjectTrackerSiteFields, ProjectTrackerStatuses, SetProjectTrackerSetup } from './bindings';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { projectBackend } from '../transport/entityIndex';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { onBackendStatusChange } from './transportStatus.svelte';
import { onTrackerSetupUpdated } from './trackerBoardEvents';
import type { Regroups } from './statusMarkGroup';

/** A held read: its answer, whether it is the stored one, its failure. */
abstract class HeldRead<T> {
  value = $state<T | null>(null);
  /** A read or a save answered, and nothing failed since. */
  current = $state(false);
  /** The last read's failure, cleared by the next read that answers. */
  error = $state<unknown>(null);
  holders = 0;
  /** Bumped by every read start and every save, so only the newest lands. */
  protected generation = 0;
  /** The newest read while it runs. */
  protected reading: Promise<void> | null = null;

  constructor(readonly project: string) {}

  protected abstract call(): Promise<T>;

  /** The project's computer, when this window knows it. */
  backend(): BackendKey | undefined {
    return projectBackend(this.project);
  }

  read(): Promise<void> {
    const reading = this.readOnce();
    this.reading = reading;
    void reading.finally(() => {
      if (this.reading === reading) this.reading = null;
    });
    return reading;
  }

  private async readOnce(): Promise<void> {
    const generation = ++this.generation;
    const backend = this.backend();
    if (backend === HOME_BACKEND) await pageGrantsResolved();
    if (generation !== this.generation) return;
    if (backend !== undefined && !hasScope('tracker:read', backend)) {
      this.error = new Error("This device may not read this project's tracker setup on its computer.");
      this.current = false;
      return;
    }
    try {
      const value = await this.call();
      if (generation !== this.generation) return;
      this.settle(value);
    } catch (err) {
      if (generation !== this.generation) return;
      // A dropped connection included: the reconnect reads again, and
      // until a read answers nothing is built from this value.
      this.error = err;
      this.current = false;
    }
  }

  protected settle(value: T): void {
    this.value = value;
    this.error = null;
    this.current = true;
  }
}

class HeldSetup extends HeldRead<SetupView> {
  saving = $state(false);
  /** Each site's statuses the setup regroups, by `siteKey`. */
  readonly regroups = $derived(regroupsOf(this.value));
  /** A change was told while a save ran: read once it has settled. */
  readOwed = false;

  protected call(): Promise<SetupView> {
    return ProjectTrackerSetup(this.project);
  }

  async save(edit: SetupEdit): Promise<SetupSaveOutcome> {
    if (this.saving) throw new Error("The project's tracker setup is still saving; try again once it has saved.");
    // Reserved before the first await: a second save waiting on the same
    // read would otherwise send too.
    this.saving = true;
    try {
      if (!this.current) await (this.reading ?? this.read());
      const view = this.value;
      if (!this.current || !view) throw this.error ?? new Error("The project's tracker setup could not be read; try again once it shows.");
      return await this.send(view, edit);
    } finally {
      this.saving = false;
      if (this.readOwed) {
        this.readOwed = false;
        void this.read();
      }
    }
  }

  private async send(view: SetupView, edit: SetupEdit): Promise<SetupSaveOutcome> {
    const generation = ++this.generation;
    try {
      const answer = await SetProjectTrackerSetup(this.project, view.revision, edit(view.setup));
      if (generation === this.generation) this.settle(answer.stored);
      if (answer.saved) return { kind: 'saved' };
      // The computer names why with every refusal (setupChangedSentence).
      return { kind: 'refused', sentence: answer.refused ?? "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now" };
    } catch (err) {
      // The file may hold the change or not: read what it holds.
      if (generation === this.generation) {
        this.current = false;
        this.readOwed = true;
      }
      throw err;
    }
  }
}

class HeldStatuses extends HeldRead<ProjectStatuses> {
  protected call(): Promise<ProjectStatuses> {
    return ProjectTrackerStatuses(this.project);
  }
}

class HeldSiteFields extends HeldRead<PinField[]> {
  constructor(project: string, readonly tracker: string, readonly site: string) {
    super(project);
  }

  protected call(): Promise<PinField[]> {
    return ProjectTrackerSiteFields(this.project, this.tracker, this.site);
  }
}

const setups = new SvelteMap<string, HeldSetup>();
const statuses = new SvelteMap<string, HeldStatuses>();
const fields = new SvelteMap<string, HeldSiteFields>();

function acquire<E extends HeldRead<unknown>>(map: SvelteMap<string, E>, key: string, make: () => E): () => void {
  let entry = map.get(key);
  if (!entry) {
    entry = make();
    map.set(key, entry);
  }
  entry.holders++;
  if (entry.holders === 1) void entry.read();
  const mine = entry;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    mine.holders--;
    if (mine.holders === 0 && map.get(key) === mine) map.delete(key);
  };
}

/** A project's setup as a surface shows it. */
export interface TrackerSetupState {
  /** The setup as last read or saved; null until a read answers. */
  readonly view: SetupView | null;
  /** An edit can be built from the view: it is the stored one. */
  readonly current: boolean;
  /** The last read's failure; null when it answered. */
  readonly error: unknown;
  readonly saving: boolean;
}

const NO_SETUP: TrackerSetupState = { view: null, current: false, error: null, saving: false };

/** Holds `project`'s setup while the caller shows it, reading it now; the
 * returned function releases the hold. */
export function acquireTrackerSetup(project: string): () => void {
  return acquire(setups, project, () => new HeldSetup(project));
}

/** `project`'s setup as held; nothing while none is held. Reactive. */
export function trackerSetupOf(project: string): TrackerSetupState {
  const entry = setups.get(project);
  return entry ? { view: entry.value, current: entry.current, error: entry.error, saving: entry.saving } : NO_SETUP;
}

/** The statuses `project`'s setup puts in another group than the
 * tracker's on `tracker`'s site `site`, while its setup is held and read;
 * none otherwise. Reactive. */
export function trackerRegroupsOf(project: string, tracker: string, site: string): Regroups | undefined {
  return setups.get(project)?.regroups.get(siteKey(tracker, site));
}

function siteKey(tracker: string, site: string): string {
  return `${tracker}\u0000${site}`;
}

function regroupsOf(view: SetupView | null): ReadonlyMap<string, Regroups> {
  const out = new Map<string, Regroups>();
  for (const entry of view?.setup.status_groups ?? []) {
    out.set(siteKey(entry.tracker, entry.site), new Map(Object.entries(entry.statuses) as [string, string][]));
  }
  return out;
}

/** Reads `project`'s setup again if a surface holds it. */
export function refreshTrackerSetup(project: string): Promise<void> {
  return setups.get(project)?.read() ?? Promise.resolve();
}

/** A change of a setup: the whole setup to save, built from the held one. */
export type SetupEdit = (setup: Setup) => Setup;

/** A save's outcome, as the computer answered it. */
export type SetupSaveOutcome =
  | { readonly kind: 'saved' }
  /** The file moved since it was read: nothing was saved, and the held
   * view is the one stored. `sentence` is the computer's, as Go words it
   * (lower case, no closing period). */
  | { readonly kind: 'refused'; readonly sentence: string };

/** Saves `edit` of `project`'s held setup. Rejects with the computer's
 * error when the save failed, whatever the file holds after it, and while
 * an earlier save of the project runs. */
export async function editTrackerSetup(project: string, edit: SetupEdit): Promise<SetupSaveOutcome> {
  const release = acquireTrackerSetup(project);
  try {
    return await setups.get(project)!.save(edit);
  } finally {
    release();
  }
}

/** The groups a status may be put in, as the setup names them. */
export const STATUS_GROUPS = ['backlog', 'todo', 'in_progress', 'done', 'canceled', 'left_out'] as const;
export type StatusGroup = (typeof STATUS_GROUPS)[number];

/** `setup` with status `status` of a site in `group`, or back in the
 * tracker's group (its entry removed) for null; a site left regrouping
 * nothing is dropped. */
export function withStatusGroup(setup: Setup, tracker: string, site: string, status: string, group: StatusGroup | null): Setup {
  const entries = setup.status_groups ?? [];
  const at = entries.findIndex((e) => e.tracker === tracker && e.site === site);
  const statuses: { [id: string]: string | undefined } = { ...(at >= 0 ? entries[at].statuses : {}) };
  if (group === null) delete statuses[status];
  else statuses[status] = group;
  const others = entries.filter((_, i) => i !== at);
  return { ...setup, status_groups: Object.keys(statuses).length > 0 ? [...others, { tracker, site, statuses }] : others };
}

/** A project's statuses as the Status groups and Starting work pages
 * show them. */
export interface TrackerStatusesState {
  readonly sites: readonly SiteStatuses[] | null;
  readonly places: readonly StartPlace[] | null;
  readonly error: unknown;
}

const NO_STATUSES: TrackerStatusesState = { sites: null, places: null, error: null };

/** Holds `project`'s statuses while the caller shows them, reading them
 * now; the returned function releases the hold. */
export function acquireTrackerStatuses(project: string): () => void {
  return acquire(statuses, project, () => new HeldStatuses(project));
}

/** `project`'s statuses as held. Reactive. */
export function trackerStatusesOf(project: string): TrackerStatusesState {
  const entry = statuses.get(project);
  return entry ? { sites: entry.value?.sites ?? null, places: entry.value?.places ?? null, error: entry.error } : NO_STATUSES;
}

/** Reads `project`'s statuses again if a surface holds them. */
export function refreshTrackerStatuses(project: string): Promise<void> {
  return statuses.get(project)?.read() ?? Promise.resolve();
}

/** A site's fields to pin as the Fields page shows them. */
export interface TrackerSiteFieldsState {
  readonly fields: readonly PinField[] | null;
  readonly error: unknown;
}

const NO_FIELDS: TrackerSiteFieldsState = { fields: null, error: null };

/** The held read of `site`'s fields for `project`'s page; `holds` names
 * what it was read under (the connections that reach the site), so a
 * change of them is a new read. */
function siteFieldsKey(project: string, tracker: string, site: string, holds: string): string {
  return `${project}\u0000${tracker}\u0000${site}\u0000${holds}`;
}

/** Holds `site`'s fields to pin for `project` while the caller shows them,
 * reading them now; the returned function releases the hold. A new
 * `holds` is a new read. */
export function acquireTrackerSiteFields(project: string, tracker: string, site: string, holds: string): () => void {
  return acquire(fields, siteFieldsKey(project, tracker, site, holds), () => new HeldSiteFields(project, tracker, site));
}

/** `site`'s fields to pin for `project` as held. Reactive. */
export function trackerSiteFieldsOf(project: string, tracker: string, site: string, holds: string): TrackerSiteFieldsState {
  const entry = fields.get(siteFieldsKey(project, tracker, site, holds));
  return entry ? { fields: entry.value, error: entry.error } : NO_FIELDS;
}

/** Reads `site`'s fields for `project` again if a surface holds them. */
export function refreshTrackerSiteFields(project: string, tracker: string, site: string, holds: string): Promise<void> {
  return fields.get(siteFieldsKey(project, tracker, site, holds))?.read() ?? Promise.resolve();
}

/** An Agents setting of a project: Change tickets, or Ask before
 * changing several at once. */
export type AgentRule = keyof AgentRules;

/** Whether the setup's Agents settings turn `rule` on: absent is on, the
 * app's built-in rule. */
export function agentRuleOn(setup: Setup | null | undefined, rule: AgentRule): boolean {
  return setup?.agents?.[rule] !== false;
}

/** `setup` with Agents setting `rule` on or off. The computer stores a
 * rule turned on as none (trackerapp's validateSetup). */
export function withAgentRule(setup: Setup, rule: AgentRule, on: boolean): Setup {
  return { ...setup, agents: { ...setup.agents, [rule]: on } };
}

/** Reads again every held entry of a project on `backend`, or of a project
 * whose computer this window does not know. A setup being saved is read
 * once its save settles: a read sent beside the save may answer the file
 * before the save's write and land after its answer, and the change told
 * may be one made after the save's write, which its answer lacks. */
function readAgainOn(backend: BackendKey): void {
  for (const entry of setups.values()) {
    const owner = entry.backend();
    if (owner !== undefined && owner !== backend) continue;
    if (entry.saving) entry.readOwed = true;
    else void entry.read();
  }
  for (const entry of statuses.values()) {
    const owner = entry.backend();
    if (owner === undefined || owner === backend) void entry.read();
  }
}

onTrackerSetupUpdated(readAgainOn);

onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  readAgainOn(backend);
  for (const entry of fields.values()) {
    const owner = entry.backend();
    if (owner === undefined || owner === backend) void entry.read();
  }
});

export function resetTrackerSetupForTest(): void {
  setups.clear();
  statuses.clear();
  fields.clear();
}
