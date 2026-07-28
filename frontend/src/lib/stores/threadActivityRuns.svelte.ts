// Per-pane registry for activity runs: stable identity, collapse
// overrides, inner scroll snapshots, the mounted row window, and pending
// jump-into-run focus requests.
//
// Runs are not stored entities. They are recomputed from scratch on every
// projection pass out of whatever items happen to be loaded, so the
// registry is the only thing that carries a run across passes.
//
// Identity is the load-bearing part. No member id is stable: lazy
// older-paging extends a run backward (new first member) and live-window
// pruning trims its head — and the prune fires mid-stream on exactly the
// long runs this feature exists to bound. Keying on any member would
// remount the row and recreate its scroll controller mid-turn. So the
// registry mints an id once and migrates it by membership: an entry
// sharing any member with the run being built lends that run its id.
//
// A run can also vanish entirely and come back — the live-window prune can
// take every item a head run had, and a thread switch drops the lot — so
// entries carrying explicit state are ARCHIVED on their way out and revived
// by either edge member (see `archiveEntry` for what that cannot recognize).
// Without that, collapsing a noisy run and then
// paging (or switching threads and returning) hands back a default run, which
// is the run-level form of the position loss `utils/threadScrollSnapshots.ts`
// exists to prevent.
//
// Session-only: the archive is per pane and dies with it. The durable layer
// is the `activityRunDefault` setting.

import type {
  ActivityRunIdentity,
  ActivityRunResolution,
} from '../utils/activityRunGrouping';
import type {
  ActivityRunFocusRequest,
  ActivityRunMountWindow,
} from '../utils/activityRunWindow';

export interface ActivityRunScrollSnapshot {
  scrollTop: number;
  /** The user scrolled up inside, so the run must not re-pin under them. */
  escaped: boolean;
}

export interface ThreadActivityRunsOptions {
  /** `expanded` | `collapsed` from user settings. Read per resolve. */
  defaultCollapsed(): boolean;
  /** `activityRunWindowRows` — how many tail rows a run mounts by default. */
  windowRows(): number;
}

export interface ThreadActivityRuns extends ActivityRunIdentity {
  /**
   * Bumps whenever a value this registry resolves onto a run node could differ. The projection
   * pass runs untracked (it walks every node and would otherwise re-run on
   * every streaming delta), so it reads this to know when a rebuild is
   * owed. Scroll snapshots deliberately do NOT bump it: they change on
   * every inner scroll frame and nothing on the node depends on them.
   */
  readonly revision: number;
  /**
   * Set whether the run renders without its clip.
   *
   * Takes the target state rather than toggling, because the caller is the only
   * one who knows the state being toggled FROM: a run with no override renders
   * expanded while it is live regardless of the defaults (see `collapsedFor`),
   * so a registry-side toggle would read the settled answer and hand back the
   * state the reader is already looking at.
   */
  setCollapsed(runId: string, collapsed: boolean): void;
  /**
   * What a run with no explicit state does in this thread: the bulk override
   * if one has been taken, otherwise the `activityRunDefault` setting.
   *
   * The header's collapse-all control renders from this rather than from a
   * survey of the runs, so the button means one thing at all times. A survey
   * would have to answer "all of WHICH runs" — only the loaded window holds
   * any — and would flip its own label as older history paged in.
   */
  readonly bulkCollapsed: boolean;
  /**
   * Collapse or expand every run in this thread, now and as more load.
   *
   * Sets the thread's default AND drops the per-run overrides it contradicts,
   * including archived ones: a bulk action the next revived run silently
   * ignored would not be "all". A run the reader toggles afterwards takes its
   * own override again, on top of this.
   */
  setAllCollapsed(collapsed: boolean): void;
  scrollSnapshot(runId: string): ActivityRunScrollSnapshot | null;
  saveScrollSnapshot(runId: string, snapshot: ActivityRunScrollSnapshot): void;
  /**
   * Runs still rendering open because they opened while live and nobody has
   * answered for them since — the candidates the timeline's auto-collapse
   * gate walks (`components/chat/timelineActivityRunAutoCollapse.ts`).
   * Includes the run that is STILL live: the registry cannot know liveness
   * (see `collapsedFor`), so the gate filters on `node.live`.
   */
  openedLiveRunIds(): string[];
  /**
   * Let a settled run take the thread's defaults again, dropping the
   * open-because-live hold `collapsedFor` recorded. Called by the
   * auto-collapse gate once the reader is provably elsewhere — never from a
   * click, which goes through `setCollapsed` and beats this hold outright.
   *
   * When the defaults say collapsed this IS the auto-collapse: the run's
   * next projection renders it as a chip, and its inner position is
   * forgotten the same way a clicked collapse forgets it. When they say
   * expanded, nothing visible changes — the hold is simply retired, so a
   * later default flip treats the run like any other settled one.
   */
  releaseOpenedLive(runId: string): void;
  /**
   * RESIZE the run's mounted window, recording the size as an explicit
   * override that no longer tracks the `activityRunWindowRows` setting. Set by
   * the "N earlier" / "N later" boundaries, which is what the user asking for
   * more rows means.
   *
   * A caller that only wants to MOVE the window uses `setWindowAnchor`.
   * Passing the size the run already has would pin it here — a short run that
   * mounts all five of its rows would stay at five as it grows.
   */
  setMountWindow(runId: string, window: ActivityRunMountWindow): void;
  /**
   * Pin the window's head to `anchorItemId`, or pass null to release it back
   * to following the run's tail. Size is untouched: this answers WHERE the
   * window sits, not how big it is.
   *
   * Owned by the run's row, which is the only place that knows whether the
   * reader is still scrolled up inside it (`ActivityRun.svelte`). A window
   * that keeps following the tail under an escaped reader drops one head row
   * per appended row, sliding what they are reading up the clip.
   */
  setWindowAnchor(runId: string, anchorItemId: string | null): void;
  /**
   * The item the window is pinned to, or null when it follows the run's tail.
   *
   * Deliberately outside the `revision` graph, like `scrollSnapshot`: its one
   * reader is the row's controller effect, which would otherwise tear down and
   * rebuild the spring every time the pin it sets moves.
   */
  windowAnchor(runId: string): string | null;
  /**
   * Ask the run's row to bring `itemId` into view once it is mounted. Held
   * on the entry rather than passed down a prop because the row may not
   * exist yet — the jump that requests it is usually what scrolls the run
   * into the virtualizer's buffer in the first place.
   *
   * Returns false when no run holds that id — a pass swept it, or `clear()`
   * did. Reported rather than silent because the caller's whole gesture is
   * void in that case: `revealActivityRunItem` relays it so a jump cannot
   * announce success for a run that will never scroll.
   */
  requestFocus(runId: string, request: ActivityRunFocusRequest): boolean;
  /** Read and clear the pending focus request. */
  takeFocus(runId: string): ActivityRunFocusRequest | null;
  /** Drop everything — thread switch. */
  clear(): void;
}

interface RunEntry {
  /**
   * The thread this run's items belong to. Taken from the items at resolve
   * time rather than read from the pane, because the archive is written
   * during `clear()` — by which point the pane may already point at the
   * incoming thread, and an entry filed under it would be revived by the
   * wrong one.
   */
  threadId: string;
  members: Set<string>;
  collapsed: boolean | null;
  /**
   * The run currently has a clip on screen to record a position from.
   *
   * Not the inverse of the entry's `collapsed` override, which is why it is
   * stored: a run whose collapse resolves from the defaults renders open
   * while it is live and while `openedLive` holds it, and only `collapsedFor`
   * sees the whole resolution — so `collapsedFor` records its own answer
   * here on every pass. `forgetInnerPosition` closes it eagerly on the same
   * flush as the collapse that makes the clip go away, so a save arriving
   * between the collapse and the next projection is refused too.
   *
   * Starts true, so an entry nobody has collapsed behaves exactly as it did
   * when this was `!collapsed`. Only a mounted clip can produce a position to
   * save, so an entry that starts permissive cannot let a wrong one through —
   * an entry that started closed could only ever drop a right one.
   *
   * Never archived. It describes a DOM state, and a run coming back from the
   * archive has no rows.
   */
  clipOpen: boolean;
  /**
   * The run rendered open because it was LIVE, with nobody having answered
   * for it — recorded by `collapsedFor` at the moment it resolves that way.
   *
   * This is what keeps a settled run from snapping shut the instant its
   * closing prose arrives: the flag outlives liveness, so the run keeps
   * rendering open until the timeline's auto-collapse gate releases it
   * (`releaseOpenedLive`) once the reader is provably elsewhere — or until
   * the reader answers directly (`setCollapsed` / `setAllCollapsed`), which
   * clears it, because an explicit answer beats a hold that exists only to
   * avoid moving things under them.
   *
   * Never archived, deliberately: a run revived after a sweep or a thread
   * switch mounts fresh rows the reader is not looking at, so it follows the
   * defaults like any other settled run.
   */
  openedLive: boolean;
  scroll: ActivityRunScrollSnapshot | null;
  /** null → the `activityRunWindowRows` setting. */
  windowRows: number | null;
  /** null → the run's tail. */
  windowStartItemId: string | null;
  focus: ActivityRunFocusRequest | null;
}

/**
 * A swept entry's state, plus the thread-qualified keys it can be found
 * again by. Item ids are unique only WITHIN a thread — the store's primary
 * key is `(thread_id, item_id)` and synthesized ids like `think:0:0` recur
 * in every thread — so a bare item id would let one thread's first run
 * revive another's state on a switch.
 */
interface ArchivedRun extends Omit<RunEntry, 'members' | 'focus' | 'clipOpen' | 'openedLive'> {
  keys: string[];
}

function archiveKey(threadId: string, itemId: string): string {
  // NUL separator: provider-issued item ids are opaque, so the one byte
  // neither a thread id nor an item id can contain is the only safe joiner.
  return `${threadId}\u0000${itemId}`;
}

/**
 * How many archive keys to keep — two per run, so ~64 runs. Small because
 * each record is four scalars and two ids; a pending focus request is
 * deliberately not among them, since a jump that never landed is stale by
 * the time its run comes back.
 */
const ARCHIVE_MAX_KEYS = 128;

function emptyEntry(threadId: string): RunEntry {
  return {
    threadId,
    members: new Set(),
    collapsed: null,
    clipOpen: true,
    openedLive: false,
    scroll: null,
    windowRows: null,
    windowStartItemId: null,
    focus: null,
  };
}

function rowIndexOfMember(
  rowMemberIds: readonly (readonly string[])[],
  itemId: string,
): number {
  for (let row = 0; row < rowMemberIds.length; row += 1) {
    if (rowMemberIds[row].includes(itemId)) return row;
  }
  return -1;
}

export function createThreadActivityRuns(
  options: ThreadActivityRunsOptions,
): ThreadActivityRuns {
  const entries = new Map<string, RunEntry>();
  // Reverse index so migration is one lookup per member instead of a scan
  // over every entry. Rebuilt incrementally as entries take new members.
  const runIdByMember = new Map<string, string>();
  // Swept entries, reachable by the first and last member each run had, keyed
  // per thread. Two keys because both edges move independently: the older
  // prune takes a run's first members and the recent prune takes its last, and
  // either one surviving is enough to recognize the run when it comes back.
  // Insertion order is the eviction order.
  const archive = new Map<string, ArchivedRun>();
  let claimed = new Set<string>();
  let nextRunId = 1;

  // The thread's own collapse default, or null to follow the setting. Set by
  // the header's collapse-all control and reset on thread switch: it is a view
  // action taken on a thread ("I don't want to read activity in THIS one"),
  // and carrying it into an unrelated thread would surprise.
  let bulkCollapsed = $state<boolean | null>(null);

  // Collapse state and the mount window both ride on the projected node, so
  // both have to be able to trigger a rebuild; a focus request bumps it too,
  // because a jump can target an item the current window already holds and
  // would otherwise change nothing the row could notice. Each moves only on
  // a deliberate user action (toggle the run, mount a chunk, jump to a hit),
  // so the rebuild is rare. Scroll snapshots are excluded on purpose — they
  // move every inner scroll frame and nothing on the node reads them.
  let revision = $state(0);

  // Creation lives here and nowhere else. A run comes into existence by being
  // projected, so every id a caller can hold already has an entry; a mutator
  // that minted one instead would seed state for whichever run later happens
  // to mint that id, and would resurrect an entry `clear()` just archived.
  function ensureEntry(runId: string, threadId: string): RunEntry {
    let entry = entries.get(runId);
    if (!entry) {
      entry = emptyEntry(threadId);
      entries.set(runId, entry);
    }
    return entry;
  }

  function indexMembers(
    entry: RunEntry,
    runId: string,
    rowMemberIds: readonly (readonly string[])[],
  ): void {
    for (const id of entry.members) {
      if (runIdByMember.get(id) === runId) runIdByMember.delete(id);
    }
    entry.members = new Set();
    for (const row of rowMemberIds) {
      for (const id of row) {
        entry.members.add(id);
        runIdByMember.set(id, runId);
      }
    }
  }

  function beginPass(): void {
    claimed = new Set();
  }

  function claimRunId(
    rowMemberIds: readonly (readonly string[])[],
    threadId: string,
  ): string {
    // Earliest matching member wins. That is what makes a split deterministic:
    // the entry follows the sub-run holding its previous first member, and the
    // other sub-run starts fresh from the setting default. On a merge the
    // earliest-positioned entry survives and the later one is swept.
    for (const row of rowMemberIds) {
      for (const id of row) {
        const candidate = runIdByMember.get(id);
        if (candidate && !claimed.has(candidate)) return candidate;
      }
    }
    // A live entry always beats an archived one — it is the same run still
    // going — so the archive is only consulted once the live scan comes up
    // empty, and over the whole membership rather than per member.
    const minted = `r${nextRunId}`;
    nextRunId += 1;
    for (const row of rowMemberIds) {
      for (const id of row) {
        const revived = archive.get(archiveKey(threadId, id));
        if (!revived) continue;
        for (const key of revived.keys) archive.delete(key);
        entries.set(minted, {
          threadId,
          members: new Set(),
          collapsed: revived.collapsed,
          clipOpen: true,
          openedLive: false,
          scroll: revived.scroll,
          windowRows: revived.windowRows,
          windowStartItemId: revived.windowStartItemId,
          focus: null,
        });
        return minted;
      }
    }
    return minted;
  }

  /**
   * Park an entry's state where a later pass can find it again. Entries with
   * nothing explicit on them are dropped: reviving one would hand back the
   * same defaults a fresh entry produces, at the cost of an archive slot a
   * real override could have used.
   *
   * Only the run's FIRST and LAST member can find it again. A reload that
   * lands on neither — a jump into the middle of a long run, whose window
   * loads only interior rows — gets a default run instead. Accepted: keying
   * every member would scale the archive with run length, and the loss is one
   * collapse override, recoverable with one click.
   */
  function archiveEntry(entry: RunEntry): void {
    if (
      entry.collapsed === null
      && entry.windowRows === null
      && entry.windowStartItemId === null
      && entry.scroll === null
    ) {
      return;
    }
    const ids = [...entry.members];
    if (ids.length === 0) return;
    const keys = [...new Set([
      archiveKey(entry.threadId, ids[0]),
      archiveKey(entry.threadId, ids[ids.length - 1]),
    ])];
    const record: ArchivedRun = {
      keys,
      threadId: entry.threadId,
      collapsed: entry.collapsed,
      scroll: entry.scroll,
      windowRows: entry.windowRows,
      windowStartItemId: entry.windowStartItemId,
    };
    for (const key of keys) {
      archive.delete(key);
      archive.set(key, record);
    }
    while (archive.size > ARCHIVE_MAX_KEYS) {
      const oldest = archive.keys().next().value;
      if (oldest === undefined) break;
      archive.delete(oldest);
    }
  }

  function resolve(
    rowMemberIds: readonly (readonly string[])[],
    threadId: string,
  ): ActivityRunResolution {
    const runId = claimRunId(rowMemberIds, threadId);
    claimed.add(runId);
    const entry = ensureEntry(runId, threadId);
    indexMembers(entry, runId, rowMemberIds);
    // A run never mounts more than it has. The stored size only exists once
    // the user has pulled in another chunk or a jump has relocated the
    // window; until then it tracks the current setting, so changing the
    // setting applies on the next pass.
    const rows = Math.min(
      rowMemberIds.length,
      Math.max(1, entry.windowRows ?? options.windowRows()),
    );
    const tailFrom = rowMemberIds.length - rows;
    let mountedFrom = tailFrom;
    if (entry.windowStartItemId !== null) {
      const anchored = rowIndexOfMember(rowMemberIds, entry.windowStartItemId);
      if (anchored < 0) {
        // The anchor row is gone — an older-side prune took it, or a
        // payloadKind flip split the run and it landed on the other side.
        // There is nothing left to hold, so the window follows the tail again.
        entry.windowStartItemId = null;
      } else {
        // Clamped so a late anchor still mounts a full window. Deliberately
        // NOT released when the tail catches up: an anchor means "the reader
        // is up here", which is a fact about the reader, not about geometry.
        // `ActivityRun.svelte` releases it when they return to the clip's
        // bottom — that is what resumes tail-following for a jumped-into or
        // scrolled-up run.
        mountedFrom = Math.min(anchored, tailFrom);
      }
    }
    return {
      runId,
      mountedFrom,
      mountedRows: rows,
    };
  }

  function endPass(): void {
    for (const [runId, entry] of [...entries]) {
      if (claimed.has(runId)) continue;
      archiveEntry(entry);
      for (const id of entry.members) {
        if (runIdByMember.get(id) === runId) runIdByMember.delete(id);
      }
      entries.delete(runId);
    }
  }

  function threadDefaultCollapsed(): boolean {
    revision;
    return bulkCollapsed ?? options.defaultCollapsed();
  }

  function collapsedFor(runId: string, live: boolean): boolean {
    const entry = entries.get(runId);
    const collapsed = resolveCollapsed(entry, live);
    // The resolved answer IS clip presence — the row renders its clip from
    // exactly this — and only this resolution sees all three inputs, so it
    // records its own answer rather than having the row report back what it
    // was just told (see the field's declaration).
    if (entry) entry.clipOpen = !collapsed;
    return collapsed;
  }

  function resolveCollapsed(entry: RunEntry | undefined, live: boolean): boolean {
    // An answer about THIS run beats everything, including liveness. A reader
    // who collapses the run they are watching means now, not when it finishes —
    // the clip closing is the only evidence the click did anything.
    const override = entry?.collapsed;
    if (override !== null && override !== undefined) return override;
    // Nobody has answered for it, so a working run shows its work — recorded,
    // because rendering open is a commitment that outlives the turn: snapping
    // shut on the exact frame the run settles would remove a viewport of
    // content in front of whoever was watching it stream. This is the whole
    // reason the parameter exists: the registry cannot know liveness, which is
    // a claim about items it never reads, and it is deliberately an input to
    // the FALLBACK only.
    if (live) {
      if (entry) entry.openedLive = true;
      return false;
    }
    // Settled, but it opened as a live run and nobody has reconciled that yet.
    // It keeps rendering open until the timeline's auto-collapse gate releases
    // it off-screen (`releaseOpenedLive`) or the reader answers directly.
    if (entry?.openedLive) return false;
    // The defaults — the thread's bulk state and the `activityRunDefault`
    // setting — say how a run should SIT, and this one has settled into them.
    return threadDefaultCollapsed();
  }

  function setCollapsed(runId: string, collapsed: boolean): void {
    const entry = entries.get(runId);
    if (!entry) return;
    // An explicit answer retires the open-because-live hold even when the
    // rendered state does not change: the hold exists to avoid moving things
    // under a reader, and the reader just spoke. A run still live re-records
    // it if this override is later dropped while it is still filling.
    entry.openedLive = false;
    if (entry.collapsed === collapsed) return;
    entry.collapsed = collapsed;
    if (collapsed) forgetInnerPosition(entry);
    else entry.clipOpen = true;
    revision += 1;
  }

  /**
   * Drop where the reader was INSIDE a run, because the run is becoming a
   * chip and there is no inside any more.
   *
   * Both halves say the same thing. The scroll offset is what the clip
   * restores on expand, and keeping it would reopen the run mid-list — at an
   * offset the reader last saw before deciding they were done with it, which
   * on a live run is behind everything that has arrived since. The window
   * anchor is the pin that says "the reader is up here"; leaving it would hold
   * the mounted window away from the tail while the clip sits at its bottom,
   * so the newest activity would not even be mounted to land on. An expanded
   * run therefore opens where a never-scrolled one does: its newest row, which
   * is the reason it is on screen.
   *
   * NOT the row-count override. That is how many rows the reader asked to
   * mount, which the chip does not contradict, and re-collapsing a run should
   * not quietly undo chunks they paged in.
   */
  function forgetInnerPosition(entry: RunEntry): void {
    entry.scroll = null;
    entry.windowStartItemId = null;
    // Said here rather than at each call site because every caller means the
    // same thing by it: the clip is going away. A run whose clip survives the
    // collapse (it still holds the tail) re-states that from its mounted row,
    // which is the only place that can know.
    entry.clipOpen = false;
  }

  return {
    get revision() {
      return revision;
    },
    beginPass,
    resolve,
    collapsedFor,
    endPass,
    setCollapsed,
    get bulkCollapsed() {
      return threadDefaultCollapsed();
    },
    setAllCollapsed: (collapsed) => {
      bulkCollapsed = collapsed;
      // Per-run overrides are dropped rather than overwritten so the runs go
      // back to following the thread, which is what makes a later flip apply
      // to them too. Collapsing also forgets each run's inner position, for
      // the same reason a single collapse does. Open-because-live holds are
      // retired the same way `setCollapsed` retires them — "all" includes the
      // settled runs those holds were keeping open — and the run that is
      // STILL live simply re-records its hold on the next pass, which is what
      // keeps a bulk collapse from blinding the reader to work in flight.
      for (const entry of entries.values()) {
        entry.collapsed = null;
        entry.openedLive = false;
        if (collapsed) forgetInnerPosition(entry);
        else entry.clipOpen = true;
      }
      // Archived runs are the ones a bulk action would otherwise miss: they
      // come back as the reader pages, and an override from before the action
      // would revive against it.
      for (const record of archive.values()) {
        record.collapsed = null;
        if (collapsed) {
          record.scroll = null;
          record.windowStartItemId = null;
        }
      }
      revision += 1;
    },
    scrollSnapshot: (runId) => entries.get(runId)?.scroll ?? null,
    saveScrollSnapshot: (runId, snapshot) => {
      // A row torn down AFTER its registry was cleared has nothing to save
      // into: `clear()` already archived the last per-frame snapshot, and
      // creating an entry here would leave a memberless ghost behind.
      const entry = entries.get(runId);
      if (!entry) return;
      // A run with no clip has no inner position to record, and the row that
      // just lost its clip tears it down THROUGH this method — so the refusal
      // has to live here rather than at the one call site. Without it the
      // teardown would write back the offset `forgetInnerPosition` just
      // dropped (0, since a detached element reports no scroll), and every
      // collapse-then-expand would reopen the run at its first row.
      //
      // `clipOpen`, not `collapsed`: those said the same thing until a
      // collapsed run began keeping its clip open while it holds the tail. A
      // live run refused here would be reset to its newest row the moment it
      // stopped being live, yanking a reader who had scrolled up inside it.
      if (!entry.clipOpen) return;
      entry.scroll = snapshot;
    },
    openedLiveRunIds: () => {
      const held: string[] = [];
      for (const [runId, entry] of entries) {
        if (entry.openedLive) held.push(runId);
      }
      return held;
    },
    releaseOpenedLive: (runId) => {
      const entry = entries.get(runId);
      if (!entry?.openedLive) return;
      entry.openedLive = false;
      // No override check: a hold only exists while nobody has answered —
      // `resolveCollapsed` records it under a null override and every
      // `setCollapsed` write retires it — so the defaults alone decide here.
      // Only an actual auto-collapse rebuilds and forgets. With the defaults
      // saying expanded the run keeps rendering exactly as it was — dropping
      // its inner position then would yank a still-open clip to its tail, and
      // a revision bump would rebuild the projection to change nothing.
      if (threadDefaultCollapsed()) {
        forgetInnerPosition(entry);
        revision += 1;
      }
    },
    setMountWindow: (runId, window) => {
      const entry = entries.get(runId);
      if (!entry) return;
      if (entry.windowRows === window.rows
        && entry.windowStartItemId === window.startItemId) return;
      entry.windowRows = window.rows;
      entry.windowStartItemId = window.startItemId;
      revision += 1;
    },
    setWindowAnchor: (runId, anchorItemId) => {
      const entry = entries.get(runId);
      if (!entry || entry.windowStartItemId === anchorItemId) return;
      entry.windowStartItemId = anchorItemId;
      revision += 1;
    },
    windowAnchor: (runId) => entries.get(runId)?.windowStartItemId ?? null,
    requestFocus: (runId, request) => {
      const entry = entries.get(runId);
      if (!entry) return false;
      entry.focus = request;
      revision += 1;
      return true;
    },
    takeFocus: (runId) => {
      const entry = entries.get(runId);
      if (!entry?.focus) return null;
      const request = entry.focus;
      // Deliberately silent: the row consuming a request must not schedule
      // another rebuild, and nothing on the node carries the request.
      entry.focus = null;
      return request;
    },
    clear: () => {
      // The incoming thread must see no live entry from the outgoing one, but
      // their state is archived on the way out, under thread-qualified keys
      // (see `archiveKey`) so only the thread it came from can revive it.
      // Returning to a thread in this pane finds its runs as it left them.
      for (const entry of entries.values()) archiveEntry(entry);
      entries.clear();
      runIdByMember.clear();
      claimed = new Set();
      // The bulk override is scoped to the thread it was taken on (see its
      // declaration), so the incoming thread starts from the setting again.
      bulkCollapsed = null;
      revision += 1;
    },
  };
}
