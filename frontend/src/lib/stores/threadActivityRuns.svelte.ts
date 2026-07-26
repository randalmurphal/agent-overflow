// Per-pane registry for activity runs: stable identity, collapse
// overrides, inner scroll snapshots, and the mounted tail-window size.
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
// Session-only, deliberately matching item-expansion leases. The durable
// layer is the `activityRunDefault` setting.

import type { ActivityRunIdentity } from '../utils/activityRunGrouping';

export interface ActivityRunScrollSnapshot {
  scrollTop: number;
  /** The user scrolled up inside; the tail-window must not trim under them. */
  escaped: boolean;
}

export interface ThreadActivityRunsOptions {
  /** `expanded` | `collapsed` from user settings. Read per resolve. */
  defaultCollapsed(): boolean;
  /** `activityRunWindowRows` — how many tail rows a run mounts by default. */
  windowRows(): number;
}

export interface ThreadActivityRuns extends ActivityRunIdentity {
  /** Explicit user state, or the setting default when never touched. */
  isCollapsed(runId: string): boolean;
  setCollapsed(runId: string, collapsed: boolean): void;
  toggleCollapsed(runId: string): void;
  scrollSnapshot(runId: string): ActivityRunScrollSnapshot | null;
  saveScrollSnapshot(runId: string, snapshot: ActivityRunScrollSnapshot): void;
  /** Rows currently mounted from the run's tail. Grows on "N earlier". */
  mountedRows(runId: string, fallback: number): number;
  setMountedRows(runId: string, rows: number): void;
  /** Drop everything — thread switch. */
  clear(): void;
}

interface RunEntry {
  members: Set<string>;
  collapsed: boolean | null;
  scroll: ActivityRunScrollSnapshot | null;
  mountedRows: number | null;
}

function emptyEntry(): RunEntry {
  return { members: new Set(), collapsed: null, scroll: null, mountedRows: null };
}

export function createThreadActivityRuns(
  options: ThreadActivityRunsOptions,
): ThreadActivityRuns {
  const entries = new Map<string, RunEntry>();
  // Reverse index so migration is one lookup per member instead of a scan
  // over every entry. Rebuilt incrementally as entries take new members.
  const runIdByMember = new Map<string, string>();
  let claimed = new Set<string>();
  let nextRunId = 1;

  // Collapse overrides are the only registry state the UI renders from, so
  // they are the only part that needs to be reactive. Scroll snapshots and
  // mounted-row counts are read imperatively at mount/trim time — making
  // them $state would churn dependents on every inner scroll event.
  let collapseRevision = $state(0);

  function entryFor(runId: string): RunEntry {
    let entry = entries.get(runId);
    if (!entry) {
      entry = emptyEntry();
      entries.set(runId, entry);
    }
    return entry;
  }

  function indexMembers(runId: string, memberItemIds: readonly string[]): void {
    const entry = entryFor(runId);
    for (const id of entry.members) {
      if (runIdByMember.get(id) === runId) runIdByMember.delete(id);
    }
    entry.members = new Set(memberItemIds);
    for (const id of memberItemIds) runIdByMember.set(id, runId);
  }

  function beginPass(): void {
    claimed = new Set();
  }

  function resolve(
    memberItemIds: readonly string[],
    childRowCount: number,
  ): { runId: string; collapsed: boolean; mountedRows: number } {
    // Earliest matching member wins. That is what makes a split deterministic:
    // the entry follows the sub-run holding its previous first member, and the
    // other sub-run starts fresh from the setting default. On a merge the
    // earliest-positioned entry survives and the later one is swept.
    let runId: string | null = null;
    for (const id of memberItemIds) {
      const candidate = runIdByMember.get(id);
      if (candidate && !claimed.has(candidate)) {
        runId = candidate;
        break;
      }
    }
    if (!runId) {
      runId = `r${nextRunId}`;
      nextRunId += 1;
    }
    claimed.add(runId);
    indexMembers(runId, memberItemIds);
    const entry = entryFor(runId);
    // A run never mounts more than it has. The stored override only exists
    // once the user has pulled in an older chunk; until then the window
    // tracks the current setting, so changing it applies on the next pass.
    const requested = entry.mountedRows ?? options.windowRows();
    return {
      runId,
      collapsed: isCollapsed(runId),
      mountedRows: Math.min(childRowCount, Math.max(1, requested)),
    };
  }

  function endPass(): void {
    for (const [runId, entry] of [...entries]) {
      if (claimed.has(runId)) continue;
      for (const id of entry.members) {
        if (runIdByMember.get(id) === runId) runIdByMember.delete(id);
      }
      entries.delete(runId);
    }
  }

  function isCollapsed(runId: string): boolean {
    collapseRevision;
    return entries.get(runId)?.collapsed ?? options.defaultCollapsed();
  }

  function setCollapsed(runId: string, collapsed: boolean): void {
    const entry = entryFor(runId);
    if (entry.collapsed === collapsed) return;
    entry.collapsed = collapsed;
    collapseRevision += 1;
  }

  function toggleCollapsed(runId: string): void {
    setCollapsed(runId, !isCollapsed(runId));
  }

  return {
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
    mountedRows: (runId, fallback) => entries.get(runId)?.mountedRows ?? fallback,
    setMountedRows: (runId, rows) => {
      entryFor(runId).mountedRows = rows;
    },
    clear: () => {
      entries.clear();
      runIdByMember.clear();
      claimed = new Set();
      collapseRevision += 1;
    },
  };
}
