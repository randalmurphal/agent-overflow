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
// by the same membership rule. Without that, collapsing a noisy run and then
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
   * Bumps whenever a value `resolve` returns could differ. The projection
   * pass runs untracked (it walks every node and would otherwise re-run on
   * every streaming delta), so it reads this to know when a rebuild is
   * owed. Scroll snapshots deliberately do NOT bump it: they change on
   * every inner scroll frame and nothing on the node depends on them.
   */
  readonly revision: number;
  /** Explicit user state, or the setting default when never touched. */
  isCollapsed(runId: string): boolean;
  setCollapsed(runId: string, collapsed: boolean): void;
  toggleCollapsed(runId: string): void;
  scrollSnapshot(runId: string): ActivityRunScrollSnapshot | null;
  saveScrollSnapshot(runId: string, snapshot: ActivityRunScrollSnapshot): void;
  /**
   * Move or resize the run's mounted window. Set by the "N earlier" /
   * "N later" boundaries and by a jump landing inside the run.
   */
  setMountWindow(runId: string, window: ActivityRunMountWindow): void;
  /**
   * Ask the run's row to bring `itemId` into view once it is mounted. Held
   * on the entry rather than passed down a prop because the row may not
   * exist yet — the jump that requests it is usually what scrolls the run
   * into the virtualizer's buffer in the first place.
   */
  requestFocus(runId: string, request: ActivityRunFocusRequest): void;
  /** Read and clear the pending focus request. */
  takeFocus(runId: string): ActivityRunFocusRequest | null;
  /** Drop everything — thread switch. */
  clear(): void;
}

interface RunEntry {
  members: Set<string>;
  collapsed: boolean | null;
  scroll: ActivityRunScrollSnapshot | null;
  /** null → the `activityRunWindowRows` setting. */
  windowRows: number | null;
  /** null → the run's tail. */
  windowStartItemId: string | null;
  focus: ActivityRunFocusRequest | null;
}

/** A swept entry's state, plus the member ids it can be found again by. */
interface ArchivedRun extends Omit<RunEntry, 'members' | 'focus'> {
  keys: string[];
}

/**
 * How many archive keys to keep — two per run, so ~64 runs. Small because
 * each record is four scalars and two ids; a pending focus request is
 * deliberately not among them, since a jump that never landed is stale by
 * the time its run comes back.
 */
const ARCHIVE_MAX_KEYS = 128;

function emptyEntry(): RunEntry {
  return {
    members: new Set(),
    collapsed: null,
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
  // Swept entries, reachable by the first and last member id each run had.
  // Two keys because both edges move independently: the older prune takes a
  // run's first members and the recent prune takes its last, and either one
  // surviving is enough to recognize the run when it comes back. Insertion
  // order is the eviction order.
  const archive = new Map<string, ArchivedRun>();
  let claimed = new Set<string>();
  let nextRunId = 1;

  // Collapse state and the mount window both ride on the projected node, so
  // both have to be able to trigger a rebuild; a focus request bumps it too,
  // because a jump can target an item the current window already holds and
  // would otherwise change nothing the row could notice. Each moves only on
  // a deliberate user action (toggle the run, mount a chunk, jump to a hit),
  // so the rebuild is rare. Scroll snapshots are excluded on purpose — they
  // move every inner scroll frame and nothing on the node reads them.
  let revision = $state(0);

  function entryFor(runId: string): RunEntry {
    let entry = entries.get(runId);
    if (!entry) {
      entry = emptyEntry();
      entries.set(runId, entry);
    }
    return entry;
  }

  function indexMembers(
    runId: string,
    rowMemberIds: readonly (readonly string[])[],
  ): void {
    const entry = entryFor(runId);
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

  function claimRunId(rowMemberIds: readonly (readonly string[])[]): string {
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
        const revived = archive.get(id);
        if (!revived) continue;
        for (const key of revived.keys) archive.delete(key);
        entries.set(minted, {
          members: new Set(),
          collapsed: revived.collapsed,
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
   */
  function archiveEntry(entry: RunEntry): void {
    if (entry.collapsed === null && entry.windowRows === null && entry.scroll === null) {
      return;
    }
    const ids = [...entry.members];
    if (ids.length === 0) return;
    const keys = [...new Set([ids[0], ids[ids.length - 1]])];
    const record: ArchivedRun = {
      keys,
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
  ): ActivityRunResolution {
    const runId = claimRunId(rowMemberIds);
    claimed.add(runId);
    indexMembers(runId, rowMemberIds);
    const entry = entryFor(runId);
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
      if (anchored >= 0 && anchored < tailFrom) {
        mountedFrom = anchored;
      } else {
        // Either the anchor row has left the loaded window, or the window has
        // caught up with the run's last row. Both mean nothing is hidden
        // below it, so it goes back to following the tail — which is how a
        // jumped-into run resumes showing new activity.
        entry.windowStartItemId = null;
      }
    }
    return {
      runId,
      collapsed: isCollapsed(runId),
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

  function isCollapsed(runId: string): boolean {
    revision;
    return entries.get(runId)?.collapsed ?? options.defaultCollapsed();
  }

  function setCollapsed(runId: string, collapsed: boolean): void {
    const entry = entryFor(runId);
    if (entry.collapsed === collapsed) return;
    entry.collapsed = collapsed;
    revision += 1;
  }

  function toggleCollapsed(runId: string): void {
    setCollapsed(runId, !isCollapsed(runId));
  }

  return {
    get revision() {
      return revision;
    },
    beginPass,
    resolve,
    endPass,
    isCollapsed,
    setCollapsed,
    toggleCollapsed,
    scrollSnapshot: (runId) => entries.get(runId)?.scroll ?? null,
    saveScrollSnapshot: (runId, snapshot) => {
      entryFor(runId).scroll = snapshot;
    },
    setMountWindow: (runId, window) => {
      const entry = entryFor(runId);
      if (entry.windowRows === window.rows
        && entry.windowStartItemId === window.startItemId) return;
      entry.windowRows = window.rows;
      entry.windowStartItemId = window.startItemId;
      revision += 1;
    },
    requestFocus: (runId, request) => {
      entryFor(runId).focus = request;
      revision += 1;
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
      // their state is archived on the way out: item ids are unique per
      // thread, so a revival can only ever match the thread it came from, and
      // returning to a thread in this pane finds its runs as it left them.
      for (const entry of entries.values()) archiveEntry(entry);
      entries.clear();
      runIdByMember.clear();
      claimed = new Set();
      revision += 1;
    },
  };
}
