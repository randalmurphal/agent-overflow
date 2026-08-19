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

import { compositeKey } from '../utils/compositeKey';
import type {
  ActivityRunIdentity,
  ActivityRunResolution,
} from '../utils/activityRunGrouping';
import type {
  ActivityRunFocusRequest,
  ActivityRunMountWindow,
} from '../utils/activityRunWindow';
import { SvelteMap } from 'svelte/reactivity';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { withViewportBottomHeld } from './threadPaneShared';
import type { PaneScrollController } from './threadPaneShared';

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
  /**
   * The pane's scroll controller, read per mutation (it registers after this
   * factory runs and churns on thread switches — same "declared later" closure
   * as the pane's other getters). The collapse mutators below run their writes
   * inside `withViewportBottomHeld` on whatever this answers, so holding the
   * viewport is the registry's job, not a convention its callers remember —
   * the reason the hold moved in here from three call sites (chat/AGENTS.md
   * "Every collapse/expand", incident 2026-08-17).
   */
  scrollController(): PaneScrollController | null;
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
   * expanded while it is the newest revealed run regardless of the defaults
   * (see `collapsedFor`), so a registry-side toggle would read the settled
   * answer and hand back the state the reader is already looking at.
   *
   * The write runs inside `withViewportBottomHeld` (the reader ASKED for this
   * height change, so the delta opens upward, instantly, over rows they are
   * already reading). Callers therefore must NOT wrap this in a hold of their
   * own — nesting issues a second restore token for nothing. The one expand
   * that owns its viewport instead is the jump's, `expandForReveal`.
   */
  setCollapsed(runId: string, collapsed: boolean): void;
  /**
   * Expand for a jump into the run — same write as
   * `setCollapsed(runId, false)` but with NO viewport hold, and the absence
   * is load-bearing, not a preference: `scrollToItem`
   * (`timelineRestore.svelte.ts`) takes a restore token before revealing and
   * aborts if the token has moved when it resumes, and a hold ISSUES a token
   * (`nextRestoreToken`, via `preserveViewportBottom`) — so routing this
   * through `setCollapsed` would cancel every jump into a collapsed run at
   * its own guard. Even without that, a bottom restore would fight the
   * viewport the jump is about to claim. Only `revealActivityRunItem`
   * (utils/activityRunWindow.ts) should call this — it is the one door for
   * jumps, and this verb can only expand, so the hold-free path cannot be
   * borrowed for a collapse.
   */
  expandForReveal(runId: string): void;
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
   * Includes the run that still holds the revealed tail: the registry
   * cannot know tail-ness (see `collapsedFor`), so the gate filters on
   * `node.atTail`.
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
   *
   * Takes the batch because the gate releases everything eligible at once,
   * and the whole batch runs inside ONE `withViewportBottomHeld` with
   * `takeover: 'yield'`: the change is UNASKED, so when a structural append
   * lands in the flushes between the change and its restore, the restore
   * stands down and the armed spring glides the new row in instead of a
   * bottom write landing it instantly (bug-report-20260731T141600Z;
   * regression: appendAfterQuiet.browser.test.ts). The transaction only
   * opens when the batch actually collapses something: a batch with no
   * releasable id, or one whose releases change no geometry (defaults say
   * expanded), pauses no spring and burns no restore token.
   */
  releaseOpenedLive(runIds: readonly string[]): void;
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
   * Whether the run holds `itemId` RIGHT NOW.
   *
   * For callers holding a projected `ActivityRunNode` that may predate a
   * prune or a sweep — a jump is the one such path — so they can tell a stale
   * node from a broken one before writing an anchor derived from it. Writing
   * a stale id is coerced to tail-follow and REPORTED as a contract break by
   * `resolvableAnchor`, which is the right answer for every writer that reads
   * from a live node and the wrong one for a writer that cannot.
   */
  containsMember(runId: string, itemId: string): boolean;
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
  /**
   * How many times a member of `runId` has had a summary-relevant field
   * change (see `activityRunSummaryFieldsChanged`). The header keys its
   * summary on this, so a reveal tick — which replaces the member's item
   * object ~50 times a second without touching those fields — costs one
   * number comparison instead of a walk over every member.
   *
   * Per run on purpose. A pane-global revision would move on any row's
   * every write and drag every run's summary along with it, which is the
   * cost this replaces, not a smaller version of it.
   */
  memberContentRevision(runId: string): number;
  /**
   * Record that `itemId`'s summary fields changed. A no-op for an id no
   * run holds, so the pane's write chokepoints can call it unconditionally
   * rather than each deciding whether a row is in a run.
   */
  noteMemberContentChanged(itemId: string): void;
  /**
   * Bumped whenever the pane replaces its item array WHOLESALE (a paged
   * load, a cache paint reconciled by `SyncThreadWindow`, a window prune).
   * Folded into the header's summary key alongside the per-run signals.
   *
   * A wholesale replace can change every summary-relevant field on rows
   * whose run membership is identical — the cache paint that shows a tool
   * as running, reconciled a moment later by the attested answer that says
   * it completed. `indexMembers` sees the same ordered ids and
   * `noteMemberContentChanged` never runs (nothing went through the
   * per-item write path), so without this the header would keep rendering
   * the stale summary until the run's membership happened to move.
   *
   * Deliberately pane-global and O(1): unlike a streaming delta, a
   * wholesale replace happens at settle/prune cadence, so re-summarizing
   * every mounted header then is the correct trade — the walk it costs is
   * bounded by what is on screen and it happens a handful of times per
   * thread visit.
   */
  readonly wholesaleGeneration: number;
  /** Record a wholesale item-array replacement. See `wholesaleGeneration`. */
  noteWholesaleReplace(): void;
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
  /**
   * Bumped by `indexMembers` when the ORDERED membership changes — a join,
   * a leave, a swap, or a reorder of the same ids. Plain, not `$state`: it
   * is written during the projection pass (which runs untracked and must
   * not touch reactive state) and read out of the same pass onto the node,
   * where consumers pick it up as a prop.
   */
  membershipEpoch: number;
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
   * The run rendered open because it was the timeline's newest revealed
   * activity — live, or with its closing prose still behind the reveal gate —
   * with nobody having answered for it. Recorded by `collapsedFor` at the
   * moment it resolves that way.
   *
   * This is what keeps a settled run from snapping shut the instant its
   * closing prose arrives: the flag outlives tail-ness, so the run keeps
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
// `membershipEpoch` is excluded with the DOM-shaped fields: a revived run
// has no members yet, so its epoch restarts at 0 and the first `resolve`
// pass stamps it — carrying the old number would claim a membership the
// entry cannot describe.
interface ArchivedRun extends Omit<
  RunEntry,
  'members' | 'focus' | 'clipOpen' | 'openedLive' | 'membershipEpoch'
> {
  keys: string[];
}

function archiveKey(threadId: string, itemId: string): string {
  return compositeKey(threadId, itemId);
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
    membershipEpoch: 0,
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

/**
 * The window anchor a run will actually hold, given what it currently
 * contains. `null` means "follow the tail".
 *
 * Every anchor writer names `timelineNodeItemId(run.children[from])`
 * (`ActivityRun.svelte`'s escape effect, `activityRunWindow.ts`'s window
 * helpers, `revealActivityRunItem`), and that id is only resolvable back to a
 * row because `activityRunGrouping.ts#buildRun` yields it into
 * `rowMemberIds[from]`. That is a CONTRACT across two files and every
 * `TimelineNode` kind, and nothing but this check enforces it.
 *
 * When it breaks the failure is not a wrong window, it is an infinite
 * rebuild: `resolve()` cannot find the anchor, nulls it, the effect sees the
 * moved window and rewrites the same id, `revision` bumps, the projection
 * rebuilds, forever — one full run-node rebuild per lap with no paint
 * between. Coercing the unresolvable anchor to the tail HERE closes that
 * loop structurally: the second write matches the stored `null` and returns
 * without bumping `revision`, so a broken node kind degrades to a
 * tail-following window instead of wedging the renderer.
 *
 * Nothing legitimate reaches the report. An anchor row lost to an older-side
 * prune or a payloadKind split is dropped by `resolve()` a pass later, when
 * the run genuinely no longer contains it; a write naming an id the run does
 * not contain RIGHT NOW can only be the contract breaking.
 */
/**
 * Runs already reported by `resolvableAnchor`, so a broken node kind reports
 * ONCE rather than on every re-assertion.
 *
 * The write that trips this is an `$effect` re-asserting the row's escape
 * anchor, which runs on every reveal tick — measured as a report per tick
 * before this existed, which buries the finding under its own repetition and
 * burns the ui-trace rotation budget. Entries only ever arrive for a run whose
 * contract is broken, so in a healthy app this set stays empty; the cap bounds
 * it anyway, because a systematically broken node kind would otherwise add one
 * entry per run for the life of the page. Past the cap the guard still coerces
 * the anchor — it just stops reporting, and the first entries carry all the
 * signal.
 */
const reportedAnchorRuns = new Set<string>();
const MAX_REPORTED_ANCHOR_RUNS = 100;

function resolvableAnchor(
  entry: RunEntry,
  runId: string,
  anchorItemId: string | null,
): string | null {
  if (anchorItemId === null || entry.members.has(anchorItemId)) return anchorItemId;
  // Keyed by thread too: `runId`s are minted per registry, so `run-1` in one
  // pane is a different run from `run-1` in another.
  const key = compositeKey(entry.threadId, runId);
  if (!reportedAnchorRuns.has(key) && reportedAnchorRuns.size < MAX_REPORTED_ANCHOR_RUNS) {
    reportedAnchorRuns.add(key);
    // Constant message, variables in `detail`: an item id and a member count
    // in the message would mint a fresh signature per run, which is unbounded
    // map growth in the capture pipeline AND a walk around its per-signature
    // cap. Console too — a remote session cannot persist the record
    // (`ReportFrontendErrorBatch` is LocalOnly), and the console line is then
    // the only surviving evidence.
    const detail = `anchor "${anchorItemId}" not in run ${runId} (${entry.members.size} members)`;
    console.warn(`[threadActivityRuns] unresolvable window anchor; tail-follow (${detail})`);
    reportFrontendDiagnostic(
      'threadActivityRuns: window anchor is not a member of its run — a TimelineNode kind\'s ' +
        'rowMemberIds does not contain its own timelineNodeItemId; falling back to tail-follow',
      detail,
    );
  }
  return null;
}

/** Test hook: the once-per-run report ledger is module state. */
export function resetActivityRunAnchorReportsForTest(): void {
  reportedAnchorRuns.clear();
}

export function createThreadActivityRuns(
  options: ThreadActivityRunsOptions,
): ThreadActivityRuns {
  const entries = new Map<string, RunEntry>();
  // Reverse index so migration is one lookup per member instead of a scan
  // over every entry. Rebuilt incrementally as entries take new members.
  const runIdByMember = new Map<string, string>();
  // Per-run content revision, keyed by runId. Reactive per KEY (SvelteMap),
  // so a member changing inside one run does not invalidate the headers of
  // the other runs on screen. Entries are dropped with their run in
  // `endPass`/`clear`; a missing key reads as 0, which is also what a run
  // that has never seen a member change should report.
  const memberContentRevisions = new SvelteMap<string, number>();
  // See `wholesaleGeneration` on the interface. Reactive and pane-global,
  // which the per-run revisions deliberately are not — this one only moves
  // at settle/prune cadence.
  let wholesaleGeneration = $state(0);
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
    // Built before the old set is torn down so membership can be compared:
    // the header stamps its summary with `membershipEpoch` rather than
    // walking the ids, and a swap that preserved the count would otherwise
    // be invisible to it.
    //
    // Compared POSITIONALLY, not as a set. `Set` iterates in insertion
    // order and insertion here follows row order, so the stored set IS the
    // ordered membership — and the summary reads it that way: the running
    // label is the LAST active member in iteration order, so the same ids
    // arriving in a different order is a different summary. A set-equality
    // test would have skipped the bump and left the header naming a tool
    // that is no longer the one in flight.
    const next = new Set<string>();
    const previous = entry.members.values();
    let changed = false;
    for (const row of rowMemberIds) {
      for (const id of row) {
        // Deduped first, so the walk compares the same sequence the set
        // stores; a repeated id must not advance the old iterator twice.
        if (next.has(id)) continue;
        next.add(id);
        if (!changed && previous.next().value !== id) changed = true;
      }
    }
    // A shrink whose survivors kept their order matches position for
    // position and would otherwise read as unchanged; anything left in the
    // old iterator is a member this pass dropped.
    if (!changed && previous.next().done !== true) changed = true;
    for (const id of entry.members) {
      if (runIdByMember.get(id) === runId) runIdByMember.delete(id);
    }
    entry.members = next;
    for (const id of next) runIdByMember.set(id, runId);
    if (changed) entry.membershipEpoch += 1;
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
          membershipEpoch: 0,
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
        // Not a contract violation: an anchor that was never resolvable is
        // refused at the write (`resolvableAnchor`), so reaching here means
        // the run really did lose a row it used to have.
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
      membershipEpoch: entry.membershipEpoch,
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
      // Not archived: the revision only means "something changed since the
      // last render", and a run coming back from the archive re-renders
      // from scratch anyway. Keeping it would grow with every swept run.
      memberContentRevisions.delete(runId);
    }
  }

  function memberContentRevision(runId: string): number {
    return memberContentRevisions.get(runId) ?? 0;
  }

  function noteMemberContentChanged(itemId: string): void {
    const runId = runIdByMember.get(itemId);
    if (runId === undefined) return;
    memberContentRevisions.set(runId, (memberContentRevisions.get(runId) ?? 0) + 1);
  }

  function noteWholesaleReplace(): void {
    wholesaleGeneration += 1;
  }

  function threadDefaultCollapsed(): boolean {
    revision;
    return bulkCollapsed ?? options.defaultCollapsed();
  }

  function collapsedFor(runId: string, atTail: boolean): boolean {
    const entry = entries.get(runId);
    const collapsed = resolveCollapsed(entry, atTail);
    // The resolved answer IS clip presence — the row renders its clip from
    // exactly this — and only this resolution sees all three inputs, so it
    // records its own answer rather than having the row report back what it
    // was just told (see the field's declaration).
    if (entry) entry.clipOpen = !collapsed;
    return collapsed;
  }

  function resolveCollapsed(entry: RunEntry | undefined, atTail: boolean): boolean {
    // An answer about THIS run beats everything, including tail-ness. A reader
    // who collapses the run they are watching means now, not when it finishes —
    // the clip closing is the only evidence the click did anything.
    const override = entry?.collapsed;
    if (override !== null && override !== undefined) return override;
    // Nobody has answered for it, and this is the timeline's newest revealed
    // run — it shows its work, recorded, because rendering open is a
    // commitment that outlives the moment: snapping shut on the exact frame
    // the run settles (or on its very FIRST render, when the wire raced the
    // reveal and the closing prose arrived before the run's first projection
    // pass — the sampled-liveness race, 2026-08-18) would hide a viewport of
    // content the reader was watching stream. This is the whole reason the
    // parameter exists: the registry cannot know where the run sits, which is
    // a claim about nodes it never reads, and it is deliberately an input to
    // the FALLBACK only. The caller states tail-ness rather than liveness on
    // purpose — see `ActivityRunIdentity.collapsedFor`.
    if (atTail) {
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

  function writeCollapsed(runId: string, collapsed: boolean): void {
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

  function setCollapsed(runId: string, collapsed: boolean): void {
    withViewportBottomHeld(options.scrollController(), () => {
      writeCollapsed(runId, collapsed);
    });
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
    memberContentRevision,
    noteMemberContentChanged,
    get wholesaleGeneration() {
      return wholesaleGeneration;
    },
    noteWholesaleReplace,
    setCollapsed,
    expandForReveal: (runId) => {
      writeCollapsed(runId, false);
    },
    get bulkCollapsed() {
      return threadDefaultCollapsed();
    },
    setAllCollapsed: (collapsed) => withViewportBottomHeld(options.scrollController(), () => {
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
    }),
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
    releaseOpenedLive: (runIds) => {
      // The gate can hand over ids whose entries a projection pass swept
      // between capture and release; filtering here (rather than trusting
      // callers to) is what keeps "a batch with nothing to do never opens a
      // transaction" a property of the API. The transaction is not free even
      // when its change moves nothing: it pauses the spring for two frames
      // and burns a restore token — the counter thread-switch restores guard
      // on — so it must only run when pixels actually move.
      const releasable = runIds.filter((runId) => entries.get(runId)?.openedLive);
      if (releasable.length === 0) return;
      // No override check: a hold only exists while nobody has answered —
      // `resolveCollapsed` records it under a null override and every
      // `setCollapsed` write retires it — so the defaults alone decide here.
      // Only an actual auto-collapse rebuilds and forgets. With the defaults
      // saying expanded the run keeps rendering exactly as it was — dropping
      // its inner position then would yank a still-open clip to its tail, a
      // revision bump would rebuild the projection to change nothing, and a
      // viewport transaction would guard a change with no geometry in it.
      if (!threadDefaultCollapsed()) {
        for (const runId of releasable) entries.get(runId)!.openedLive = false;
        return;
      }
      withViewportBottomHeld(
        options.scrollController(),
        () => {
          for (const runId of releasable) {
            const entry = entries.get(runId)!;
            entry.openedLive = false;
            forgetInnerPosition(entry);
            revision += 1;
          }
        },
        // 'yield': nobody asked for this collapse, so a wire append landing
        // between the release and its restore hands the trip to the
        // structural spring instead of writing a bottom that already contains
        // the new row (regression: appendAfterQuiet.browser.test.ts).
        { takeover: 'yield' },
      );
    },
    setMountWindow: (runId, window) => {
      const entry = entries.get(runId);
      if (!entry) return;
      const anchor = resolvableAnchor(entry, runId, window.startItemId);
      if (entry.windowRows === window.rows
        && entry.windowStartItemId === anchor) return;
      entry.windowRows = window.rows;
      entry.windowStartItemId = anchor;
      revision += 1;
    },
    setWindowAnchor: (runId, anchorItemId) => {
      const entry = entries.get(runId);
      if (!entry) return;
      const anchor = resolvableAnchor(entry, runId, anchorItemId);
      if (entry.windowStartItemId === anchor) return;
      entry.windowStartItemId = anchor;
      revision += 1;
    },
    windowAnchor: (runId) => entries.get(runId)?.windowStartItemId ?? null,
    containsMember: (runId, itemId) => entries.get(runId)?.members.has(itemId) ?? false,
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
      memberContentRevisions.clear();
      claimed = new Set();
      // The bulk override is scoped to the thread it was taken on (see its
      // declaration), so the incoming thread starts from the setting again.
      bulkCollapsed = null;
      revision += 1;
    },
  };
}
