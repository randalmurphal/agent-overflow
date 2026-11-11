// One PR's merge-conflict read lifecycle in one checkout: what is wanted,
// what the last read settled, and the one runner that turns the first into
// the second. `prReviewConflicts.svelte.ts` owns the registry, the public
// API and the read itself.
//
// Every open and every reconcile frame overwrites `wanted` (an observation
// is never dropped for matching the read in flight). Only `run` starts
// reads, and after each settlement it decides, from `wanted` at that
// moment, whether to read again. Waiters are released by a settlement that
// covers them, by the runner stopping, or by `retire`; nothing awaits a
// promise that teardown can leave installed.

import { SvelteMap } from 'svelte/reactivity';
import type { WorkspaceRef } from '../types/git';
import type { PRDetail } from '../types/models';
import type { PRRef } from '../utils/prReference';
import { prRevisionKey, type PRRevision } from '../utils/prRevision';

export interface PRConflicts {
  treeOID: string;
  baseLabel: string;
  headLabel: string;
  paths: string[];
  /** Per-path merge-tree messages — the only signal for non-textual
   * conflicts (modify/delete, rename/rename, …); rendered as marker
   * rows at the top of that file's body. */
  notes: Partial<Record<string, string[]>>;
  /** Fallback strip: messages that name no conflicted path (rare). */
  messages: string[];
  /** The revision the read was asked at. The backend merges its head with
   * the base branch's tip as origin has it during the read, not with
   * `revision.baseSHA` (the base the forge recorded, which does not follow
   * the branch). A head or base-branch move makes the tree answer for a
   * merge nobody asked about. */
  revision: PRRevision;
}

/**
 * What a read answers for: the revision and the forge's mergeability. A
 * base branch that moves changes no revision field, but the forge reports
 * what the move did to the merge in its mergeability. Two observations with
 * one key are answered by one read.
 */
export function conflictReadKey(revision: PRRevision, detail: PRDetail): string {
  return `${prRevisionKey(revision)}\n${String(detail.mergeability ?? '')}`;
}

/** The latest observation of the PR: the snapshot to read, numbered. */
interface Observation {
  seq: number;
  ref: PRRef;
  detail: PRDetail;
  /** Null until the forge reports a revision; nothing can be read. */
  revision: PRRevision | null;
  key: string;
}

/** One read the runner runs: the observation it answers and its intent. */
export interface ConflictRead extends Observation {
  revision: PRRevision;
  /** A person's ask that no read has started for yet: the read may prompt
   * for git credentials, and its failure arms the pump's retry. */
  userInitiated: boolean;
}

/** How an observation was made. */
export type ObservationKind =
  /** A person opened the view: read even at a key already read (the base
   * tip moves without changing it), unless a read of it is in flight. */
  | 'open'
  /** A pump frame of a view on screen. A frame after a failed read is the
   * pump's retry; after a success it asks for nothing at an unchanged key. */
  | 'frame';

interface Waiter {
  seq: number;
  resolve: () => void;
}

export class PRConflictEntry {
  constructor(
    readonly prKey: string,
    /** The checkout that computes the tree. merge-tree needs a local clone,
     * and a head move recomputes without an attacher present to supply one. */
    readonly workspace: WorkspaceRef,
    /** Whether a conflict view on this entry is on screen. */
    private readonly viewOpen: () => boolean,
    /** Runs one read; never rejects. True when the tree was read. */
    private readonly readTree: (entry: PRConflictEntry, read: ConflictRead) => Promise<boolean>,
  ) {}

  state = $state<PRConflicts | null>(null);
  loading = $state(false);
  /** The merge-tree computation's own failure. */
  treeError = $state<string | null>(null);
  /** Per-path content-read failures. Per PATH because the reads run in
   * PARALLEL: one shared slot let a later file's success clear an earlier
   * file's banner while that file was still contentless. */
  readonly errorByPath = new SvelteMap<string, string>();
  readonly contentByPath = new SvelteMap<string, string>();
  /** In-flight per-path content loads, so N panes expanding the same file
   * share one read. */
  readonly inFlight = new Map<string, Promise<void>>();
  /** Bumped by every tree read and by retirement; a file read answers only
   * for the tree it was started under. */
  epoch = 0;
  /** False once the PR's last holder left: nothing reads or writes again. */
  alive = true;

  private wanted: Observation | null = null;
  private observed = 0;
  /** The observation a person last asked about, and the last one a read
   * started for: a read is the person's while the first is newer. */
  private userSeq = 0;
  private startedSeq = 0;
  /** A read must have started at or after this observation (an open). */
  private forceSeq = 0;
  /** A failed read is answered only once a read started at or after this
   * observation (the latest frame) has failed too. */
  private retrySeq = 0;
  private settled: { key: string; seq: number; ok: boolean } | null = null;
  private current: ConflictRead | null = null;
  private running = false;
  private waiters: Waiter[] = [];

  /** One banner for the surface: the tree failure if there is one, else the
   * first file that could not be read (named, since its body is missing). */
  get error(): string | null {
    if (this.treeError !== null) return this.treeError;
    const first = this.errorByPath.entries().next();
    if (first.done) return null;
    const [path, message] = first.value;
    return `${path}: ${message}`;
  }

  /** A person opened the view on this snapshot. Resolves once a read that
   * covers it settles, the runner has nothing more to read (no revision
   * yet), or the entry retires. */
  open(ref: PRRef, detail: PRDetail, revision: PRRevision | null): Promise<void> {
    const seq = this.record('open', ref, detail, revision);
    const done = new Promise<void>((resolve) => this.waiters.push({ seq, resolve }));
    void this.run();
    return done;
  }

  /** A pump frame. */
  frame(ref: PRRef, detail: PRDetail, revision: PRRevision | null): void {
    this.record('frame', ref, detail, revision);
    void this.run();
  }

  /** Overwrite what is wanted with the latest observation. */
  private record(kind: ObservationKind, ref: PRRef, detail: PRDetail, revision: PRRevision | null): number {
    const seq = ++this.observed;
    const key = revision ? conflictReadKey(revision, detail) : '';
    this.wanted = { seq, ref, detail, revision, key };
    if (kind === 'frame') {
      this.retrySeq = seq;
    } else if (revision && this.current?.key === key) {
      // The read in flight answers this open: one read per key at a time.
      this.forceSeq = this.current.seq;
    } else {
      this.forceSeq = seq;
      this.userSeq = seq;
    }
    return seq;
  }

  /** The PR's last holder left: stop, and release every waiter. */
  retire(): void {
    this.alive = false;
    this.epoch += 1;
    this.release(Infinity);
  }

  /** The read `wanted` still needs, or null when nothing should run now. */
  private next(): ConflictRead | null {
    const wanted = this.wanted;
    if (!this.alive || wanted === null || wanted.revision === null) return null;
    // A background read needs a view on screen; an open waiting on its
    // snapshot is reason enough on its own.
    if (!this.viewOpen() && this.waiters.length === 0) return null;
    const last = this.settled;
    if (last && last.key === wanted.key && last.seq >= this.forceSeq && (last.ok || last.seq >= this.retrySeq)) return null;
    return { ...wanted, revision: wanted.revision, userInitiated: this.userSeq > this.startedSeq };
  }

  private async run(): Promise<void> {
    if (this.running) return;
    this.running = true;
    try {
      for (let read = this.next(); read !== null; read = this.next()) {
        this.current = read;
        this.startedSeq = read.seq;
        this.loading = true;
        const ok = await this.readTree(this, read);
        this.current = null;
        this.settled = { key: read.key, seq: read.seq, ok };
        this.release(read.seq);
      }
    } finally {
      this.current = null;
      this.running = false;
      this.loading = false;
      this.release(Infinity);
    }
  }

  private release(through: number): void {
    const remaining: Waiter[] = [];
    for (const waiter of this.waiters) {
      if (waiter.seq <= through) waiter.resolve();
      else remaining.push(waiter);
    }
    this.waiters = remaining;
  }
}
