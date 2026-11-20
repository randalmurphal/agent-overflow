// Forge facts per PR: what the forge and the PR's repository answer about
// the names its bodies mention (utils/forgeFacts.ts), so a body paints in
// its final shape. Owned beside the PR snapshot and dropped with it
// (prReviewStore.svelte.ts calls `observePRFacts` from its `onApply` and
// `dropForgeFacts` from its `onDrop`).
//
// Work follows the snapshot, never the screen. Each applied snapshot scans
// only the bodies (description, latest reviews, comments by id) whose text
// changed since the last one, marks the keys nobody asked about `pending`
// in the same task, so a body mounting in that frame already reads
// `pending`, and asks for every key it needs in ONE `resolveForgeFacts`
// call (chunks of 256). A found answer is never asked for again by the
// clock: a frame asks again only for an answer that can change on its own,
// an unavailable one or an image whose Git LFS object is not local a
// minute after it, an absent one ten minutes after it (`dueAgain`), once
// per frame, the old answer in use meanwhile. A found label list counts as
// absent while a lex under it decided a label a body names is not in it
// (the reader's `denied`, marked on the entry until the answer changes).
// The user's reload of the PR (`reloadForgeFacts`) asks the forge again
// for everything it holds, once. Local
// kinds (commits) need the pane's workspace and loaded revision, which
// arrive after the first frame: `noteForgeFactsRevision` asks for those
// once when the revision becomes known or its head moves. Nothing here
// runs on a timer or a mount.
//
// The scan is a prefetch, not the authority on what is read. A reader
// asked for a key the PR holds no fact for (a draft or an editor preview,
// which no snapshot carries, or a key the scan did not name) is answered
// `pending` at once, and the next microtask asks for the keys taken that
// way in one call. Nothing says when such a reader is gone, so those keys
// are held until the PR goes, at most `MAX_READ_FACTS_PER_PR` of them
// (past it a new one renders by shape), and they give way to a body's
// keys, oldest first, when the PR is full.
//
// Readers get an immutable snapshot of a PR's facts per version
// (`forgeFactReader`): a document scope compares the snapshot it lexed
// under with the next one by signature, so only blocks that read a key
// whose answer changed rebuild. The live map is copied on the first write
// after a snapshot was handed out, at most once per frame or answer.
//
// A GitLab body's labels read one fact, the project's label list (at most
// 1,000 labels, bounded by the backend). It is one of the PR's facts, and
// the PRs of a project share one answer object for it (`sharedList`), so a
// list and its title index are held once however many of them are open.
//
// Bounded: a PR holds at most `MAX_FACTS_PER_PR` facts (a key past it is
// not asked for and renders by shape) and `MAX_FACT_TEXT_PER_PR` characters
// of answered file lines (an answer past it is held as unavailable, so its
// permalink stays a link, and is asked for again once text is dropped), a
// key no current body holds is dropped with the body, and everything goes
// when the PR leaves the PR store. A body holds only keys the PR holds a
// fact for; one that named keys past the bound is scanned again by the
// first frame with room. A body's text is the snapshot's own string, never
// a second copy.
//
// An answer applies only to the request that is out for its key: a key
// asked again (at another head, or dropped with its body and named again)
// takes the newer request's answer, whichever arrives first.
//
// A computer without `ResolveForgeFacts` (an older release, whose binding
// answers null) is learned once: what its PRs hold is dropped, so bodies
// render by shape, and nothing is marked or asked on it again until its
// connection changes.
//
// A body's scan is kept while its text is the same, and, when a scan that
// reads the head found keys in it (`FactScan.readsHead`), while the
// frame's revision (its head and base commits and branch names) is too.
// That revision is itself a fact the readers answer (`REVISION_FACT_KEY`),
// never asked for: a block that resolves a repository file by it rebuilds
// when it changes, and no other block does.

import { SvelteMap } from 'svelte/reactivity';
import type { BackendKey } from '../transport/backendKey';
import type { WorkspaceRef } from '../types/git';
import type { PRDetail, ReviewThread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import {
  factKind,
  isLocalFactKey,
  NO_REVISION,
  REVISION_FACT_KEY,
  sameFact,
  type FactRevision,
  type ForgeFact,
  type ForgeFactReader,
  type ForgeFacts,
} from '../utils/forgeFacts';
import { forgeFactScanner, type ScannedKeys } from '../utils/forgeReferenceExtension';
import type { PRRef } from '../utils/prReference';
import type { PRRevision } from '../utils/prRevision';
import { workspaceKeyBackend } from '../utils/workspaceKey';
import { onBackendStatusChange } from './transportStatus.svelte';
import { ResolveForgeFacts } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { prReferenceWire } from '../utils/prReference';

/** The facts one PR holds at most. */
export const MAX_FACTS_PER_PR = 4096;
/** The keys of one call at most (the wire's bound). */
export const FACTS_PER_CALL = 256;
/** The characters of answered file lines (`lines:` facts) one PR holds at most. */
export const MAX_FACT_TEXT_PER_PR = 2 * 1024 * 1024;
/** The facts of one PR held for readers alone at most (within `MAX_FACTS_PER_PR`). */
export const MAX_READ_FACTS_PER_PR = 1024;
/** An absent answer, or a found one a lex read as denying a name (a label list lacking a title), older than this is asked for again by the next frame holding it. */
export const FACT_FRESH_MS = 10 * 60 * 1000;
/** An unavailable answer, or an image whose Git LFS object is not local, is asked for again no sooner than this after it. */
export const FACT_RETRY_MS = 60 * 1000;

export type ResolveForgeFactsCall = (
  backend: BackendKey,
  workspace: WorkspaceRef | null,
  pr: PRRef,
  revision: PRRevision | null,
  keys: readonly string[],
  fresh: boolean,
) => Promise<ForgeFacts | null>;

const ZERO_REVISION: PRRevision = { baseRef: '', baseSHA: '', headSHA: '' };

// The computer's answer; null when it has no `ResolveForgeFacts`. Without
// the checkout to read in (a PR attached by no pane) nothing can be asked,
// and every key is unavailable.
const bound: ResolveForgeFactsCall = async (backend, workspace, pr, revision, keys, fresh) => {
  if (!workspace) return { facts: keys.map(UNAVAILABLE) };
  const answer: { facts?: unknown } | null = await withBackendTarget(backend, () =>
    ResolveForgeFacts(workspace, prReferenceWire(pr), revision ?? ZERO_REVISION, [...keys], fresh),
  );
  if (answer === null) return null;
  return { facts: (Array.isArray(answer.facts) ? answer.facts : []) as ForgeFact[] };
};
let resolver: ResolveForgeFactsCall = bound;

/**
 * Asks `backend` for the facts `keys` name, about `pr` at `revision` in
 * `workspace` (null and null when the pane has not loaded one: local kinds
 * answer `unavailable`); `fresh` reads the answers that can change from
 * the forge again rather than the computer's cache (a user's reload). The
 * one seam the `ResolveForgeFacts` binding plugs into; null when the
 * computer does not have it.
 */
export function resolveForgeFacts(
  backend: BackendKey,
  workspace: WorkspaceRef | null,
  pr: PRRef,
  revision: PRRevision | null,
  keys: readonly string[],
  fresh: boolean,
): Promise<ForgeFacts | null> {
  return resolver(backend, workspace, pr, revision, keys, fresh);
}

/** Test seam: answer calls with `call`, or restore the default with null. */
export function __setResolveForgeFactsForTest(call: ResolveForgeFactsCall | null): void {
  resolver = call ?? bound;
}

/** One key's state. `state` and `sig` never change in place: a snapshot
 * shares the object. The rest is bookkeeping no reader sees. */
interface Entry {
  readonly state: ForgeFact | 'pending';
  readonly sig: string;
  /** When the held answer arrived; 0 while none has. */
  at: number;
  /** The request out for it (`PRFacts.requests`), 0 when none is. */
  request: number;
  /** The head the last request ran at ('' with no revision). */
  askedHead: string;
  /** Its answer's text was past the PR's text bound: held as unavailable
   * and not asked for again until text is dropped. */
  over?: true;
  /** A lex decided from this found answer that a name it would hold does
   * not exist (a label a complete list lacks): due again as an absent
   * answer is. A changed answer is a new entry, without it. */
  denies?: true;
}

interface Body {
  /** The snapshot's own string: replaced by each frame's equal one, so the
   * store never keeps an older snapshot's copy alive. */
  text: string;
  /** The keys it names that the PR holds a fact for. */
  readonly keys: readonly string[];
  /** It names keys past the PR's bound: scanned again by a frame with room. */
  readonly partial: boolean;
  /** Its keys embed `head`: a frame at another head scans it again. */
  readonly readsHead: boolean;
  /** The frame revision it was scanned at (`revisionId`). */
  readonly head: string;
  /** The frame that last saw it. */
  seen: number;
}

interface PRFacts {
  readonly key: string;
  readonly backend: BackendKey;
  readonly ref: PRRef;
  readonly scan: (body: string, revision: FactRevision) => ScannedKeys;
  /** The revision of the last frame that named a head commit, and its id. */
  frameRevision: FactRevision;
  head: string;
  entries: Map<string, Entry>;
  /** `entries` is held by `reader`: copy before a write. */
  shared: boolean;
  reader: ForgeFactReader | null;
  version: number;
  bodies: Map<string, Body>;
  /** Keys held for readers that no body's scan named, oldest first. */
  read: Set<string> | null;
  /** Keys readers took in this task, asked for by its next microtask. */
  missed: string[] | null;
  frame: number;
  /** The requests sent: the id of the last. */
  requests: number;
  /** When a frame next has a key to ask again: 0 at once, Infinity never. */
  dueAt: number;
  /** The characters of file lines the held facts carry. */
  textChars: number;
  workspace: WorkspaceRef | null;
  revision: PRRevision | null;
}

const prs = new Map<string, PRFacts>();
/** Each PR's facts version, the one thing a reader tracks. */
const versions = new SvelteMap<string, number>();
/** Computers without `ResolveForgeFacts`: not asked again until their
 * connection changes. */
const lacking = new Set<BackendKey>();
let signatures = 0;

const PENDING_SIG = 'p';
const NO_KEYS: readonly string[] = Object.freeze([]);
const UNAVAILABLE = (key: string): ForgeFact => ({ key, status: 'unavailable' });
/** The checkout each held PR was attached from, until its revision loads. */
const workspaces = new Map<string, WorkspaceRef>();
const EMPTY_READER: ForgeFactReader = Object.freeze({ get: () => undefined, signature: () => '' });

/** Observability for tests: what the store holds and does. */
export const forgeFactsCounters = { scans: 0, calls: 0, keysAsked: 0, requestArrays: 0 };

onBackendStatusChange((backend) => {
  lacking.delete(backend);
});

const prRefId = (ref: PRRef): string => `${ref.forge}\u0000${ref.host}\u0000${ref.namespace}\u0000${ref.repo}\u0000${ref.number}`;

function entryFor(ref: PRRef, key: string): PRFacts {
  const held = prs.get(key);
  if (held && prRefId(held.ref) === prRefId(ref)) return held;
  const pr: PRFacts = {
    key,
    backend: workspaceKeyBackend(key),
    ref,
    // Keys name projects by path, never by address: the scan needs the PR
    // reference alone.
    scan: forgeFactScanner({ pr: ref, backend: workspaceKeyBackend(key), webBase: '' }),
    frameRevision: NO_REVISION,
    head: '',
    entries: new Map(),
    shared: false,
    reader: null,
    version: held ? held.version : 0,
    bodies: new Map(),
    read: null,
    missed: null,
    frame: 0,
    requests: 0,
    dueAt: Infinity,
    textChars: 0,
    workspace: held?.workspace ?? workspaces.get(key) ?? null,
    revision: held?.revision ?? null,
  };
  prs.set(key, pr);
  return pr;
}

/** The live map, unshared: copied once after a reader took a snapshot. */
function writable(pr: PRFacts): Map<string, Entry> {
  if (pr.shared) {
    pr.entries = new Map(pr.entries);
    pr.shared = false;
  }
  pr.reader = null;
  return pr.entries;
}

function bump(pr: PRFacts): void {
  pr.version++;
  versions.set(pr.key, pr.version);
}

function headOf(pr: PRFacts): string {
  return pr.revision?.headSHA ?? '';
}

/**
 * Folds an applied PR snapshot in: scans the bodies that changed and asks
 * for what they need. `ref` is the held PR reference; nothing happens
 * without one.
 */
export function observePRFacts(
  key: string,
  ref: PRRef | undefined,
  value: { readonly detail: PRDetail | null; readonly threads: readonly ReviewThread[] },
): void {
  if (!ref || (ref.forge !== 'github' && ref.forge !== 'gitlab')) return;
  const pr = entryFor(ref, key);
  if (lacking.has(pr.backend)) return;
  const frame = ++pr.frame;
  const detail = value.detail;
  let moved = false;
  if (detail?.headSHA) {
    const revision: FactRevision = { head: detail.headSHA, base: detail.baseRefSHA ?? '', headRef: detail.headRefName ?? '', baseRef: detail.baseRefName ?? '' };
    const id = revisionId(revision);
    if (id !== pr.head) {
      pr.frameRevision = revision;
      pr.head = id;
      moved = true;
    }
  }
  const head = pr.head;
  let ask: string[] | null = null;
  let marked = false;
  let changed = false;
  let seen = 0;
  const now = Date.now();
  // A body names keys past the bound.
  let partial = false;
  // Scans `text` and holds, for the body `id`, the keys the PR has a fact
  // for or has room to ask about.
  const admit = (id: string, text: string) => {
    forgeFactsCounters.scans++;
    const { keys, readsHead } = pr.scan(text, pr.frameRevision);
    let kept: string[] | null = null;
    let past = false;
    for (const factKey of keys) {
      if (pr.entries.has(factKey)) {
        // The body holds it now, whoever read it first.
        pr.read?.delete(factKey);
        (kept ??= []).push(factKey);
        continue;
      }
      // A body's key comes before one held for readers alone.
      if (pr.entries.size >= MAX_FACTS_PER_PR && !evictRead(pr)) {
        past = true;
        continue;
      }
      writable(pr).set(factKey, pendingEntry(headOf(pr)));
      (kept ??= []).push(factKey);
      marked = true;
      (ask ??= newRequest()).push(factKey);
    }
    if (past) partial = true;
    pr.bodies.set(id, { text, keys: kept ?? NO_KEYS, partial: past, readsHead, head, seen: frame });
  };
  const visit = (id: string, text: string) => {
    seen++;
    const held = pr.bodies.get(id);
    if (held && held.text === text && (!held.readsHead || held.head === head)) {
      held.text = text;
      held.seen = frame;
      if (held.partial) partial = true;
      return;
    }
    changed = true;
    admit(id, text);
  };
  if (detail) {
    visit('d', detail.body ?? '');
    for (const review of detail.latestReviews ?? []) visit(`r:${review.authorLogin}\u0000${review.submittedAt}`, review.body ?? '');
  }
  for (const thread of value.threads) {
    for (const comment of thread.comments) visit(`c:${comment.id}`, comment.body ?? '');
  }
  // A body gone takes its keys with it, unless another body holds them.
  if (seen < pr.bodies.size) {
    for (const [id, body] of pr.bodies) if (body.seen !== frame) pr.bodies.delete(id);
    dropUnheld(pr);
  }
  else if (changed) dropUnheld(pr);
  // Room again: the bodies that named keys past the bound get theirs.
  if (partial && pr.entries.size < MAX_FACTS_PER_PR) {
    for (const [id, body] of pr.bodies) {
      if (!body.partial) continue;
      if (pr.entries.size >= MAX_FACTS_PER_PR) break;
      admit(id, body.text);
    }
  }
  // Keys to ask again: unavailable ones and stale answers the bodies hold.
  if (now >= pr.dueAt) ask = askAgain(pr, now, ask);
  // Readers answer the revision from their snapshot: a moved one is a new one.
  if (moved && !marked) pr.reader = null;
  if (marked || moved) bump(pr);
  if (ask) send(pr, ask, false);
}

function pendingEntry(head: string): Entry {
  return { state: 'pending', sig: PENDING_SIG, at: 0, request: 0, askedHead: head };
}

function revisionId(revision: FactRevision): string {
  return `${revision.head}\u0000${revision.base}\u0000${revision.headRef}\u0000${revision.baseRef}`;
}

function newRequest(): string[] {
  forgeFactsCounters.requestArrays++;
  return [];
}

/** Drops each fact no current body holds and no reader asked for. */
function dropUnheld(pr: PRFacts): void {
  let held: Set<string> | null = null;
  for (const body of pr.bodies.values()) for (const key of body.keys) (held ??= new Set()).add(key);
  let entries: Map<string, Entry> | null = null;
  let freed = 0;
  for (const [key, entry] of pr.entries) {
    if (held?.has(key) || pr.read?.has(key)) continue;
    freed += textChars(entry.state);
    (entries ??= writable(pr)).delete(key);
  }
  // The held snapshot still names them; no body reads them, so no reader
  // needs a new one.
  if (freed === 0) return;
  pr.textChars -= freed;
  // Room again: the answers the bound turned away are asked for by this frame.
  for (const entry of pr.entries.values()) {
    if (!entry.over) continue;
    delete entry.over;
    entry.at = 0;
    pr.dueAt = 0;
  }
}

/** The characters of file lines `state` carries. */
function textChars(state: ForgeFact | 'pending'): number {
  return typeof state === 'object' && typeof state.text === 'string' ? state.text.length : 0;
}

/**
 * When an answer is due to be asked for again by the clock: an unavailable
 * one, or an image whose Git LFS object is not local (a pull changes it,
 * which the backend sees on its next look), a minute after it; an absent
 * one, or a found one a lex read as denying a name (a label list lacking a
 * title a body names; a label or an issue can be made), ten minutes after
 * it; any other found thing never (only `reloadForgeFacts` asks again),
 * nor one the text bound turned away.
 */
function dueAgain(entry: Entry & { state: ForgeFact }): number {
  if (entry.over) return Infinity;
  const { status } = entry.state;
  if (status === 'unavailable' || entry.state.lfs === true) return entry.at + FACT_RETRY_MS;
  if (status === 'absent' || entry.denies) return entry.at + FACT_FRESH_MS;
  return Infinity;
}

/** Adds to `ask` the held keys due again, and sets when the next one is. */
function askAgain(pr: PRFacts, now: number, ask: string[] | null): string[] | null {
  let due = Infinity;
  const head = headOf(pr);
  for (const [key, entry] of pr.entries) {
    if (entry.request !== 0 || entry.state === 'pending') continue;
    const again = dueAgain(entry as Entry & { state: ForgeFact });
    if (now >= again) {
      entry.askedHead = head;
      (ask ??= newRequest()).push(key);
    }
    else if (again < due) due = again;
  }
  pr.dueAt = due;
  return ask;
}

/**
 * The pane loaded `revision` of the PR `key` in `workspace`: when it is
 * the first one or its head moved, asks once for the local keys the
 * bodies hold that have no answer at that head.
 */
export function noteForgeFactsRevision(key: string, workspace: WorkspaceRef, revision: PRRevision): void {
  const pr = prs.get(key);
  if (!pr) return;
  const before = pr.revision;
  pr.workspace = workspace;
  pr.revision = revision;
  if (before !== null && before.headSHA === revision.headSHA) return;
  if (lacking.has(pr.backend)) return;
  const head = revision.headSHA;
  let ask: string[] | null = null;
  let marked = false;
  // Every fact held is one a body or a reader holds. A replaced entry
  // goes into the copy `writable` makes; this walk reads the map it began on.
  for (const [factKey, entry] of pr.entries) {
    if (!isLocalFactKey(factKey) || entry.askedHead === head) continue;
    // Asked at another head, or answered without a repository to read.
    if (entry.state === 'pending' || entry.request !== 0 || entry.state.status === 'unavailable') {
      entry.askedHead = head;
      (ask ??= newRequest()).push(factKey);
      // Unread for want of the checkout: asked again, it reads as asked,
      // so an image keeps its loading box rather than flashing the chip.
      if (entry.state !== 'pending') {
        writable(pr).set(factKey, pendingEntry(head));
        marked = true;
      }
    }
  }
  if (marked) bump(pr);
  if (ask) send(pr, ask, false);
}

/**
 * The user reloaded the PR `key`: asks once again for every answer it
 * holds, in the fewest calls, each `fresh`, so the computer reads what can
 * change from the forge rather than its cache. The held answers stay in use until theirs
 * arrive, and one answered the same keeps its signature, so nothing
 * rebuilds. A key already asked for, or still unanswered, is not asked
 * twice, so a second reload while the first is out asks nothing.
 */
export function reloadForgeFacts(key: string): void {
  const pr = prs.get(key);
  if (!pr || lacking.has(pr.backend)) return;
  const head = headOf(pr);
  let ask: string[] | null = null;
  for (const [factKey, entry] of pr.entries) {
    if (entry.request !== 0 || entry.state === 'pending' || entry.over) continue;
    entry.askedHead = head;
    (ask ??= newRequest()).push(factKey);
  }
  if (ask) send(pr, ask, true);
}

/** Asks for `keys` as one request, every chunk `fresh` or none: its
 * answer applies to each of them only while no later request has asked
 * for it. */
function send(pr: PRFacts, keys: string[], fresh: boolean): void {
  const request = ++pr.requests;
  for (const key of keys) {
    const entry = pr.entries.get(key);
    if (entry) entry.request = request;
  }
  for (let start = 0; start < keys.length; start += FACTS_PER_CALL) {
    const chunk = start === 0 && keys.length <= FACTS_PER_CALL ? keys : keys.slice(start, start + FACTS_PER_CALL);
    forgeFactsCounters.calls++;
    forgeFactsCounters.keysAsked += chunk.length;
    let call: Promise<ForgeFacts | null>;
    try {
      call = resolveForgeFacts(pr.backend, pr.workspace, pr.ref, pr.revision, chunk, fresh);
    } catch (err) {
      call = Promise.reject(err);
    }
    call.then(
      (answer) => (answer === null ? learnLacking(pr.backend) : settle(pr, request, chunk, answer.facts ?? [])),
      (err: unknown) => fail(pr, request, chunk, err),
    );
  }
}

/**
 * `backend` has no `ResolveForgeFacts`: its PRs drop what they hold, so
 * their bodies render by shape, as that release renders them, and nothing
 * is marked or asked there until its connection changes. Their bodies
 * are scanned again by the first snapshot after that.
 */
function learnLacking(backend: BackendKey): void {
  if (lacking.has(backend)) return;
  lacking.add(backend);
  for (const pr of prs.values()) {
    if (pr.backend !== backend) continue;
    pr.bodies.clear();
    pr.read = null;
    pr.missed = null;
    pr.dueAt = Infinity;
    if (pr.entries.size === 0) continue;
    writable(pr).clear();
    pr.textChars = 0;
    bump(pr);
  }
}

/** Applies `request`'s answer to the keys it is still the request out
 * for, and marks the ones it does not name unavailable; one version bump
 * for the lot. */
function settle(pr: PRFacts, request: number, keys: readonly string[], facts: readonly ForgeFact[]): void {
  if (prs.get(pr.key) !== pr) return;
  const byKey = new Map<string, ForgeFact>();
  for (const fact of facts) if (fact && typeof fact.key === 'string') byKey.set(fact.key, fact);
  const now = Date.now();
  let changed = false;
  for (const key of keys) {
    const entry = pr.entries.get(key);
    if (!entry || entry.request !== request) continue;
    entry.request = 0;
    let fact = byKey.get(key) ?? UNAVAILABLE(key);
    const held = entry.state;
    // Text past the PR's bound is not held: the key reads as unavailable.
    const over = pr.textChars - textChars(held) + textChars(fact) > MAX_FACT_TEXT_PER_PR;
    if (over) fact = UNAVAILABLE(key);
    // A refresh nobody could answer keeps the answer in use.
    if (fact.status === 'unavailable' && held !== 'pending' && held.status !== 'unavailable') {
      entry.at = now;
      continue;
    }
    if (held !== 'pending' && sameFact(held, fact)) {
      entry.at = now;
      if (over) entry.over = true;
      continue;
    }
    pr.textChars += textChars(fact) - textChars(held);
    if (fact.labels) fact = sharedList(pr, key, fact);
    writable(pr).set(key, { state: fact, sig: String(++signatures), at: now, request: 0, askedHead: entry.askedHead, ...(over ? { over: true as const } : {}) });
    changed = true;
  }
  pr.dueAt = Math.min(pr.dueAt, nextDue(pr));
  if (changed) bump(pr);
}

/**
 * The answer object another PR holds for `key` when it reads as `fact`:
 * every open PR of a project holds its one label list, and its index
 * (`labelIndex`), once. Answers never change in place, so sharing one is
 * safe; a PR that drops it leaves the others theirs.
 */
function sharedList(self: PRFacts, key: string, fact: ForgeFact): ForgeFact {
  for (const other of prs.values()) {
    if (other === self) continue;
    const state = other.entries.get(key)?.state;
    if (typeof state === 'object' && sameFact(state, fact)) return state;
  }
  return fact;
}

function nextDue(pr: PRFacts): number {
  let due = Infinity;
  for (const entry of pr.entries.values()) {
    if (entry.request !== 0 || entry.state === 'pending') continue;
    const again = dueAgain(entry as Entry & { state: ForgeFact });
    if (again < due) due = again;
  }
  return due;
}

function fail(pr: PRFacts, request: number, keys: readonly string[], err: unknown): void {
  if (prs.get(pr.key) !== pr) return;
  // A key a later request asked for, or one dropped since, lost nothing.
  const kinds = new Map<string, number>();
  for (const key of keys) {
    if (pr.entries.get(key)?.request === request) kinds.set(factKind(key), (kinds.get(factKind(key)) ?? 0) + 1);
  }
  if (kinds.size === 0) return;
  // Kinds and counts only: a key names projects, people and labels.
  reportFrontendDiagnostic(
    'forge facts: a request failed',
    `${[...kinds].map(([kind, count]) => `${kind} ${count}`).join(', ')}; ${err instanceof Error ? err.name : typeof err}`,
  );
  settle(pr, request, keys, []);
}

/**
 * The PR's facts as of now, for a document scope; re-evaluates where it is
 * read when that PR's facts change, and only that PR's. An empty reader
 * before the PR's first snapshot; null without a key.
 */
export function forgeFactReader(key: string | null): ForgeFactReader | null {
  if (!key) return null;
  versions.get(key);
  const pr = prs.get(key);
  if (!pr) return EMPTY_READER;
  if (pr.reader) return pr.reader;
  const entries = pr.entries;
  pr.shared = true;
  const revision: ForgeFact = { key: REVISION_FACT_KEY, status: 'found', revision: pr.frameRevision };
  const revisionSig = pr.head;
  pr.reader = {
    get: (factKey) => (factKey === REVISION_FACT_KEY ? revision : (entries.get(factKey)?.state ?? readMissed(pr, factKey))),
    signature: (factKey) => (factKey === REVISION_FACT_KEY ? revisionSig : (entries.get(factKey)?.sig ?? '')),
    denied: (factKey) => readDenied(pr, entries.get(factKey)),
  };
  return pr.reader;
}

/**
 * A lex under a snapshot decided from its found answer `entry` that a
 * name the answer would hold does not exist: the entry is due again as an
 * absent answer is (`dueAgain`), and the next frame past that asks. The
 * mark lasts while the answer does; a changed answer is a new entry, and
 * the blocks that read it lex again and mark it again if a name is still
 * missing. Runs inside a lex: it writes no reactive state.
 */
function readDenied(pr: PRFacts, entry: Entry | undefined): void {
  if (!entry || entry.denies || entry.state === 'pending' || entry.state.status !== 'found') return;
  entry.denies = true;
  // With a request out, its answer sets when the next one is due.
  if (entry.request === 0) pr.dueAt = Math.min(pr.dueAt, dueAgain(entry as Entry & { state: ForgeFact }));
}

/**
 * A reader was asked for `key`, which its snapshot holds no fact for. The
 * PR takes the key in the same call, so the read answers `pending` (the
 * body's first paint has the asked shape) and the next microtask asks for
 * it; a key the PR came to hold after the snapshot reads `pending` too,
 * and the reader's next snapshot has its answer. Undefined when the PR or
 * its readers' share is full, the PR is gone or its computer cannot
 * answer: the body renders by shape. Runs inside a lex: it writes no
 * reactive state.
 */
function readMissed(pr: PRFacts, key: string): 'pending' | undefined {
  if (prs.get(pr.key) !== pr || lacking.has(pr.backend)) return undefined;
  if (pr.entries.has(key)) return 'pending';
  if (pr.entries.size >= MAX_FACTS_PER_PR || (pr.read?.size ?? 0) >= MAX_READ_FACTS_PER_PR) return undefined;
  writable(pr).set(key, pendingEntry(headOf(pr)));
  (pr.read ??= new Set()).add(key);
  if (pr.missed === null) {
    pr.missed = [];
    queueMicrotask(() => askMissed(pr));
  }
  pr.missed.push(key);
  return 'pending';
}

function askMissed(pr: PRFacts): void {
  const missed = pr.missed;
  pr.missed = null;
  if (missed === null || prs.get(pr.key) !== pr) return;
  // Not one a frame dropped to make room for a body's key since.
  const ask = missed.filter((key) => {
    const entry = pr.entries.get(key);
    return entry !== undefined && entry.state === 'pending' && entry.request === 0;
  });
  if (ask.length === 0) return;
  bump(pr);
  send(pr, ask, false);
}

/** Makes room for a body's key by dropping the fact held longest for
 * readers alone; false when there is none. */
function evictRead(pr: PRFacts): boolean {
  if (!pr.read || pr.read.size === 0) return false;
  for (const key of pr.read) {
    pr.read.delete(key);
    const entry = pr.entries.get(key);
    if (!entry) continue;
    pr.textChars -= textChars(entry.state);
    writable(pr).delete(key);
    return true;
  }
  return false;
}

/** The checkout the PR `key` was attached from: where its facts are
 * read until a pane loads a revision. Dropped with the PR. */
export function noteForgeFactsWorkspace(key: string, workspace: WorkspaceRef | undefined): void {
  if (!workspace) return;
  workspaces.set(key, workspace);
  const pr = prs.get(key);
  if (!pr || pr.workspace !== null) return;
  pr.workspace = workspace;
  // What was unavailable for want of a checkout to ask in is due at once.
  for (const entry of pr.entries.values()) {
    if (entry.state === 'pending' || entry.state.status !== 'unavailable' || entry.over) continue;
    entry.at = 0;
    pr.dueAt = 0;
  }
}

/** The checkout the PR `key`'s facts are read in (the pane's, else the one
 * it was attached from), where its repository's images are read too; null
 * before either is known. */
export function forgeFactsWorkspace(key: string): WorkspaceRef | null {
  return prs.get(key)?.workspace ?? workspaces.get(key) ?? null;
}

/** Drops everything held for the PR `key`, with its entry in the PR store. */
export function dropForgeFacts(key: string): void {
  prs.delete(key);
  versions.delete(key);
  workspaces.delete(key);
}

/** Test seam: what the store holds for `key`, or null. */
export function __forgeFactsHeldForTest(key: string): { facts: number; bodies: number; bodyKeys: number; version: number; textChars: number } | null {
  const pr = prs.get(key);
  if (!pr) return null;
  let bodyKeys = 0;
  for (const body of pr.bodies.values()) bodyKeys += body.keys.length;
  return { facts: pr.entries.size, bodies: pr.bodies.size, bodyKeys, version: pr.version, textChars: pr.textChars };
}

/** Test seam: every PR key the store holds anything for. */
export function __forgeFactsKeysForTest(): string[] {
  return [...new Set([...prs.keys(), ...versions.keys(), ...workspaces.keys()])];
}

/** Test seam: drop everything, as a fresh module load would. */
export function __resetForgeFactsForTest(): void {
  prs.clear();
  versions.clear();
  lacking.clear();
  resolver = bound;
  workspaces.clear();
  for (const name of Object.keys(forgeFactsCounters) as (keyof typeof forgeFactsCounters)[]) forgeFactsCounters[name] = 0;
}
