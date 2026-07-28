// Transport-gap recovery event domain: coarse-grained resync when
// wsClient.ts detects a missed seq on a channel — re-fetches sidebar
// projections and the affected panes' loaded windows so SQLite (the
// authoritative history cache) backfills whatever was lost. Fan-in
// target of events.ts's setupEventListeners.
import type { Thread } from '../types/models';
import { iterPanes } from './panes.svelte';
import { GetThread } from './bindings';
import { refreshSidebarProjections, syncThreadRow } from './eventsThreadRows';
import { fetchDiscussionChannelSnapshot } from './eventsDiscussion';
import { hydrateProviderAccounts } from './eventsProvider';
import { hydrateRateLimitsSnapshots } from './eventsRateLimits';

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
          syncThreadRow(t);
        }).catch((err: unknown) => {
          console.warn(`events: refresh thread ${threadId} after provider:usage gap: ${err}`);
        });
      }
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
