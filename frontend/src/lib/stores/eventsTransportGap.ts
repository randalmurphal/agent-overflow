// Transport-gap recovery event domain: coarse-grained resync when
// wsClient.ts detects a missed seq on a channel — re-fetches sidebar
// projections and the affected panes' loaded windows so SQLite (the
// authoritative history cache) backfills whatever was lost. Fan-in
// target of events.ts's setupEventListeners.
//
// Two producers reach here, and the distinction matters for what a gap
// implies. The RECONNECT one is the server's explicit `gap:true` marker:
// the replay ring couldn't cover our cursor. The MID-CONNECTION one is
// wsClient's forward-seq-skip detection: the bus dropped events into a
// full subscriber buffer while the socket stayed up. Neither carries the
// missed range or the entity key, so every branch below is a re-fetch of
// current truth rather than a replay of what was lost.
import type { Thread } from '../types/models';
import { iterPanes } from './panes.svelte';
import { GetThread } from './bindings';
import { refreshSidebarProjections, syncThreadRow } from './eventsThreadRows';
import { clearLiveUsageSnapshot } from './threadContextWindow';
import { fetchDiscussionChannelSnapshot } from './eventsDiscussion';
import { hydrateProviderAccounts } from './eventsProvider';
import { hydrateRateLimitsSnapshots } from './eventsRateLimits';
import { resyncWorktreeSetups } from './eventsWorktreeSetup';
import { markImportConnectionLost } from './sessionImport.svelte';
import { getThreads } from './threads.svelte';
import { resyncGitStatusAfterGap } from './gitStatusStore.svelte';
import { resyncPRReviewAfterGap } from './prReviewStore.svelte';
import { resyncMcpServersAfterGap } from './mcpServers.svelte';

// The handler matches on the channel name we lost rather than each
// payload kind because a single gap on `provider:item_event` can
// straddle upserts AND deltas; refreshing the whole pane is the
// simplest correct response. (Channel semantics are documented at the
// wiring site in events.ts.)
export function applyTransportGap(gap: { channel: string; seq: number }): void {
  if (!gap || typeof gap.channel !== 'string') return;
  switch (gap.channel) {
    case 'provider:item_event':
    case 'provider:turn_started':
    case 'provider:turn_completed':
    case 'thread:updated': {
      refreshSidebarProjections();
      // Per-pane on purpose: refreshFromBackend refetches THAT
      // pane's loaded window (two panes on one thread can hold
      // different slices), so this cannot dedupe by threadId the
      // way the usage branch below does.
      for (const pane of iterPanes()) {
        if (!pane.threadId) continue;
        void pane.refreshFromBackend();
      }
      return;
    }
    case 'provider:account': {
      // A missed login/switch/removal event leaves the composer rings keyed
      // to the wrong account. Re-pull the selection; the store's generation
      // guard discards this snapshot if a newer live event lands first.
      void hydrateProviderAccounts().catch((err: unknown) => {
        console.warn(`events: refresh provider accounts after provider:account gap: ${err}`);
      });
      return;
    }
    case 'provider:usage': {
      void hydrateRateLimitsSnapshots().catch((err: unknown) => {
        console.warn(`events: refresh rate limits after provider:usage gap: ${err}`);
      });
      // refreshFromBackend doesn't pull `lastTokenUsage` from the
      // store, so a missed usage event would leave the meter stale
      // forever. Re-read each affected thread's row so
      // `seedContextWindow` rebuilds the meter from the persisted
      // snapshot (the pane replaceThread inside syncThreadRow re-runs
      // the seed via thread.svelte.ts). syncThreadRow rather than a
      // raw replaceThread because the fresh row's job here is
      // lastTokenUsage — its read/completion markers can lag live
      // local state and must merge forward, not revert. Dedupe by
      // threadId so two panes mounting the same thread don't issue
      // two RPCs for the same refresh.
      const seen = new Set<string>();
      for (const pane of iterPanes()) {
        if (!pane.threadId || seen.has(pane.threadId)) continue;
        const threadId = pane.threadId;
        seen.add(threadId);
        void GetThread(threadId).then((thread) => {
          const t = thread as Thread | null;
          if (!t) return;
          // The re-fetched DB row is the authority after a gap. Drop
          // the live usage side cache BEFORE the pane replaceThread
          // inside syncThreadRow re-seeds, or a pre-gap snapshot would
          // shadow the persisted value this refresh exists to restore.
          // Usage events arriving after this line re-populate the
          // cache with post-gap (fresher) snapshots as usual.
          clearLiveUsageSnapshot(threadId);
          syncThreadRow(t);
        }).catch((err: unknown) => {
          console.warn(`events: refresh thread ${threadId} after provider:usage gap: ${err}`);
        });
      }
      return;
    }
    // The entity channels (git:status / pr:updated / mcp:status). Each
    // emits exactly ONE frame per state change, so a dropped frame is
    // terminal for every consumer of that entity: no later frame happens
    // to repair it, and the stale value is indistinguishable from a
    // correct one (a clean worktree that isn't, a PR head that has moved
    // on, an MCP server shown connected after it dropped).
    //
    // Recovery is deliberately blanket — every live key of the store, not
    // one. The gap payload carries no entity key and cannot: the frames
    // that would have named it are the ones that never arrived. Coarse is
    // correct here rather than lazy, because the two things that would
    // make it expensive are both absent — live keys are bounded by what is
    // mounted (a handful of workspaces / PRs / panes, not the store's
    // history), and each store's re-source KEEPS its last value while the
    // fresh one loads, so a blanket resync costs a few RPCs and zero
    // flicker.
    case 'git:status':
      resyncGitStatusAfterGap();
      return;
    case 'pr:updated':
      resyncPRReviewAfterGap();
      return;
    case 'mcp:status':
      resyncMcpServersAfterGap();
      return;
    case 'system:stats':
    case 'highlight:seed':
    case 'highlight:diff_seed': {
      // The opposite of the entity channels: nothing to recover. A
      // system:stats frame carries the WHOLE host sample and the next one
      // lands within ~2s; the highlight seeds are point-in-time cache
      // warmers whose consumers fall back to the highlight RPC on a miss.
      // These are also the highest-rate channels on the wire, so they are
      // the likeliest to be the ones dropped when a subscriber buffer
      // fills — letting them reach the default branch would turn every
      // overflow into a full sidebar + pane refetch for data that repairs
      // itself.
      return;
    }
    case 'worktree:setup': {
      // The gap carries no range, so the run's snapshot is the only way back.
      // Scoped to threads whose row says a setup ran — every other thread
      // would be an RPC for a guaranteed-idle answer.
      resyncWorktreeSetups(getThreads());
      return;
    }
    case 'session-import:progress': {
      // Like the worktree case, the gap carries no range — and unlike every
      // other channel here there is no snapshot to re-fetch: an import run
      // exists only as its frame stream, so lost frames cannot be recovered
      // and the terminal `done` may be among them. The store ends the run as
      // "outcome unknown" (and resyncs the sidebar for whatever landed)
      // rather than leaving a progress bar frozen forever. A gap with no run
      // in flight is a no-op there.
      markImportConnectionLost();
      return;
    }
    case 'discussion:message':
    case 'discussion:state': {
      // A discussion channel's state is owned by the channel, not by any
      // one pane — so like the usage case above, fetch once per channelId
      // and apply the identical result to every pane hanging off that
      // channel's parent thread (split view of the same discussion),
      // rather than one GetChannelState + GetChannelMessages round trip
      // per pane. Full resync (afterSeq -1): a gap means we don't trust
      // any pane's incremental cursor.
      const seenChannelIds = new Set<string>();
      for (const pane of iterPanes()) {
        const channelId = pane.thread?.discussionId;
        if (!channelId || seenChannelIds.has(channelId)) continue;
        seenChannelIds.add(channelId);
        void fetchDiscussionChannelSnapshot(channelId).then(({ state, messages }) => {
          for (const p of iterPanes()) {
            if (p.thread?.discussionId !== channelId) continue;
            p.applyChannelState(state);
            p.applyChannelMessages(messages);
          }
        }).catch((err: unknown) => {
          console.warn(`events: refresh discussion channel ${channelId} after transport gap: ${err}`);
        });
      }
      return;
    }
    default:
      // Unknown channel: log a breadcrumb and refresh active panes
      // anyway. Refreshing is cheap; missing a refresh on a future
      // channel that needs one would be silent data drift.
      console.warn(
        `events: transport gap on unknown channel "${gap.channel}" — refreshing active panes`,
      );
      refreshSidebarProjections();
      for (const pane of iterPanes()) {
        if (!pane.threadId) continue;
        void pane.refreshFromBackend();
      }
  }
}
