<script lang="ts">
  // DesignSnapshotList — replaces the old DesignArtifactList.
  //
  // Renders the snapshot tree for a design thread. Each row shows the
  // snapshot label, a relative timestamp, and a "Branch from this"
  // affordance that calls BranchFromSnapshot to restore main/ from the
  // snapshot's stored copy. The "Snapshot" affordance at the top calls
  // CaptureSnapshot to freeze the current main/ — same behaviour as the
  // toolbar button, surfaced here so the snapshot panel is
  // self-contained when used outside the preview toolbar.
  //
  // Indentation: snapshots can chain (parent → child) when the user
  // branches from one and the agent advances. We render the tree as a
  // flat list with a 12px-per-depth indent so the lineage is readable
  // without needing a real tree control. Depth is computed by walking
  // parentSnapshotId chains; a cycle would loop forever, so we cap at
  // 16 hops and bail.
  //
  // Snapshot list comes from `pane.designSnapshots`. The preview panel
  // refreshes that list on mount and on `design:snapshots-update`; this
  // component reads it as derived state.

  import GitBranch from 'lucide-svelte/icons/git-branch';
  import Camera from 'lucide-svelte/icons/camera';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { DesignSnapshot } from '../../types/design';
  import {
    BranchFromSnapshot,
    CaptureSnapshot,
    ListDesignSnapshots,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { relativeTime } from '../../utils/format';
  import Icon from '../primitives/Icon.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let busyId = $state<string | null>(null);
  let capturing = $state(false);

  type Row = {
    snapshot: DesignSnapshot;
    depth: number;
  };

  // Build a flat row list with computed depth. The backend returns
  // newest-first which is the order the user expects; we don't re-sort.
  let rows = $derived.by<Row[]>(() => {
    const byId = new Map<string, DesignSnapshot>();
    for (const snapshot of pane.designSnapshots) {
      byId.set(snapshot.id, snapshot);
    }
    return pane.designSnapshots.map((snapshot) => {
      let depth = 0;
      let cursor: DesignSnapshot | undefined = snapshot;
      const seen = new Set<string>();
      while (
        cursor?.parentSnapshotId
        && byId.has(cursor.parentSnapshotId)
        && depth < 16
      ) {
        if (seen.has(cursor.id)) break;
        seen.add(cursor.id);
        cursor = byId.get(cursor.parentSnapshotId);
        depth += 1;
      }
      return { snapshot, depth };
    });
  });

  async function refresh(threadId: string): Promise<void> {
    try {
      const list = (await ListDesignSnapshots(threadId)) as DesignSnapshot[] | null;
      if (pane.threadId !== threadId) return;
      pane.setDesignSnapshots(Array.isArray(list) ? list : []);
    } catch (err) {
      console.warn('ListDesignSnapshots failed:', err);
    }
  }

  async function captureSnapshot(): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId || capturing) return;
    const label = window.prompt('Snapshot label (optional):', '') ?? '';
    capturing = true;
    try {
      await CaptureSnapshot(threadId, label);
      addToast('success', label ? `Snapshot "${label}" captured` : 'Snapshot captured');
      await refresh(threadId);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      addToast('error', `Snapshot failed: ${message}`);
    } finally {
      capturing = false;
    }
  }

  async function branchFrom(snapshot: DesignSnapshot): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId || busyId) return;
    if (
      !window.confirm(
        `Restore main/ from "${snapshot.label || 'untitled snapshot'}"? Current changes will be replaced.`,
      )
    ) {
      return;
    }
    busyId = snapshot.id;
    try {
      await BranchFromSnapshot(threadId, snapshot.id);
      addToast('success', 'Restored from snapshot');
      // Snapshot list is unchanged by a restore — main/ is what changed,
      // and the preview iframe will reload via design:reload-main.
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      addToast('error', `Branch failed: ${message}`);
    } finally {
      busyId = null;
    }
  }

  function rowLabel(snapshot: DesignSnapshot): string {
    if (snapshot.label) return snapshot.label;
    return snapshot.auto ? 'Auto-snapshot' : 'Untitled snapshot';
  }
</script>

<aside class="w-full flex flex-col min-h-0 bg-transparent border-l border-border-subtle">
  <div class="flex items-center justify-between px-3 pt-3 pb-2 border-b border-border-subtle shrink-0">
    <div>
      <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">
        Snapshots
      </p>
      <p class="text-[10px] text-fg-hint mt-0.5 tabular-nums">
        {rows.length}
        {rows.length === 1 ? 'snapshot' : 'snapshots'}
      </p>
    </div>
    <button
      type="button"
      onclick={() => void captureSnapshot()}
      disabled={!pane.threadId || capturing}
      title="Capture snapshot"
      class={[
        'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
        'border border-border-subtle bg-surface-0 px-2 py-1',
        'text-[12px] text-fg cursor-pointer transition-colors',
        'hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        'disabled:opacity-60 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="design-snapshot-list-capture"
    >
      <Icon icon={Camera} size={12} strokeWidth={1.6} class="shrink-0" />
      <span>{capturing ? 'Capturing…' : 'Snapshot'}</span>
    </button>
  </div>
  <div class="flex-1 min-h-0 overflow-y-auto py-1 px-1">
    {#if rows.length === 0}
      <p class="px-3 py-4 text-[12px] text-fg-subtle">No snapshots yet.</p>
    {:else}
      {#each rows as row (row.snapshot.id)}
        {@const indent = Math.min(row.depth, 8) * 12}
        <div
          class="flex items-start gap-2 px-2 py-1.5 rounded-[var(--radius-field)] hover:bg-surface-2/30 transition-colors"
          style="padding-left: {8 + indent}px;"
          data-testid="design-snapshot-row"
        >
          <div class="flex-1 min-w-0">
            <div class="flex items-center gap-1.5">
              {#if row.snapshot.auto}
                <span
                  class="text-[9px] font-semibold px-1 py-0.5 rounded-[4px] shrink-0 tracking-wide
                    bg-accent/10 text-accent"
                  aria-hidden="true"
                  title="Captured automatically"
                >
                  AUTO
                </span>
              {/if}
              <span class="text-[12px] truncate flex-1 text-fg">
                {rowLabel(row.snapshot)}
              </span>
            </div>
            <div class="text-[10px] text-fg-hint mt-0.5">
              {relativeTime(row.snapshot.createdAt, getSettings().timestampFormat)}
            </div>
          </div>
          <button
            type="button"
            onclick={() => void branchFrom(row.snapshot)}
            disabled={busyId !== null}
            title="Restore main/ from this snapshot"
            aria-label="Branch from {rowLabel(row.snapshot)}"
            class={[
              'inline-flex items-center justify-center rounded-[var(--radius-field)]',
              'p-1 text-fg-muted cursor-pointer transition-colors shrink-0',
              'hover:text-fg hover:bg-surface-2/40',
              'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
              'disabled:opacity-50 disabled:cursor-not-allowed',
            ].join(' ')}
            data-testid="design-snapshot-branch"
          >
            <Icon icon={GitBranch} size={13} strokeWidth={1.6} />
          </button>
        </div>
      {/each}
    {/if}
  </div>
</aside>
