// Pure derivation helpers for BackgroundTaskTray. Kept in utils/ so the
// grouping/pruning/sort logic can be unit-tested without mounting the
// component and so the .svelte file stays under the 300-line ceiling.

import type { Item } from '../types/models';

export interface TrayTask {
  /** Stable id used for the row key and scroll-to-item request. */
  rowId: string;
  /** Primary item the row reads summary/timing from — the launch when
   * we have one, otherwise the completion. */
  anchor: Item;
  /** The launch item, if it's still in the backend's live set. */
  launch: Item | null;
  /** The completion item, if one has landed for this launch. */
  completion: Item | null;
  /** Resolved status for the badge + pulse. */
  status: 'running' | 'completed' | 'errored' | 'declined';
  /** ms since the launch started; null when we only have a completion
   * (no meaningful start time to count from). */
  elapsedMs: number | null;
}

// The backend exposes three terminal statuses for a completion row:
// `completed`, `errored`, and `declined`. The tray used to collapse
// `declined` into `completed` (green ✓) even though the rest of the UI
// (ToolDecisionChip) renders declined as error-colored. Map it through
// faithfully so the affordance matches.
export function completionStatusFor(completion: Item): TrayTask['status'] {
  if (completion.status === 'errored') return 'errored';
  if (completion.status === 'declined') return 'declined';
  return 'completed';
}

/**
 * Group each backgrounded launch with its tool_completion sibling,
 * prune pairs whose completion has aged past `retentionMs`, and sort
 * so the most recently active pair is first.
 *
 * The backend persists the two as independent immutable rows: the
 * launch stays `status='running'` forever (spec invariant — see
 * docs/architecture/chat-rewrite.md "Background tray"), and the
 * completion is a separate row linked via `completionOf`. The tray
 * can't treat "launch.status === 'running'" as liveness — it must
 * treat a (launch, completion) pair as a single logical task and drop
 * BOTH once the completion ages past the retention window. Otherwise
 * the launch would re-render as "Running" forever after retention
 * elapsed.
 */
export function deriveTrayTasks(
  items: readonly Item[],
  now: number,
  retentionMs: number,
): TrayTask[] {
  interface Bucket {
    launch: Item | null;
    completion: Item | null;
  }
  const buckets = new Map<string, Bucket>();
  const bucketFor = (key: string): Bucket => {
    let b = buckets.get(key);
    if (!b) {
      b = { launch: null, completion: null };
      buckets.set(key, b);
    }
    return b;
  };

  for (const item of items) {
    if (item.completionOf) {
      const b = bucketFor(item.completionOf);
      // The backend upserts the same completion id in place, so a
      // duplicate is rare; the createdAt comparison is defensive
      // against any out-of-order delivery.
      if (!b.completion || b.completion.createdAt < item.createdAt) {
        b.completion = item;
      }
    } else if (item.status === 'running') {
      bucketFor(item.id).launch = item;
    }
  }

  const out: TrayTask[] = [];
  for (const { launch, completion } of buckets.values()) {
    // Drop the whole pair once the completion ages out. A completion
    // without a launch (launch already pruned from the source list)
    // still renders during the window so the user sees the final
    // state land.
    if (completion && now - completion.createdAt >= retentionMs) continue;
    const anchor = launch ?? completion;
    if (!anchor) continue;

    const status: TrayTask['status'] = completion
      ? completionStatusFor(completion)
      : 'running';

    out.push({
      rowId: anchor.id,
      anchor,
      launch,
      completion,
      status,
      // Only the launch has a meaningful "started at" timestamp. For
      // orphan completions (no launch in the list) counting elapsed
      // time from the completion's createdAt would misleadingly show
      // "0s" for a task that actually ran for minutes — so we omit
      // the label entirely in that case.
      elapsedMs: launch ? Math.max(0, now - launch.createdAt) : null,
    });
  }

  // The launch's updatedAt doesn't bump when the completion lands,
  // so a just-completed pair would otherwise sort below a launch
  // that's been running for ages. Take the max of the two so active
  // rows bubble to the top.
  return out.sort((a, b) => {
    const aAct = Math.max(a.launch?.updatedAt ?? 0, a.completion?.updatedAt ?? 0);
    const bAct = Math.max(b.launch?.updatedAt ?? 0, b.completion?.updatedAt ?? 0);
    return bAct - aAct;
  });
}
