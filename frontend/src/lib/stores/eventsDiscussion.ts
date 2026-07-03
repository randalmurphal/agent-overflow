// Discussion-channel event domain: routes the `discussion:message` /
// `discussion:state` pushes into the owning pane's channel-state layer
// (`threadChannelState.svelte.ts`, surfaced through `ThreadPane`) and
// centralizes the "resync this channel from scratch" fetch shape shared
// by ChannelView's initial load and transport-gap recovery. Fan-in
// target of events.ts's setupEventListeners.
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import type { ThreadPane } from './thread.svelte';
import { iterPanes } from './panes.svelte';
import { GetChannelMessages, GetChannelState } from './bindings';

/** Wire payload for `discussion:message`. `threadId` is the PARENT
 * thread id (`channel.ThreadID`) — ChannelView/DiscussionView are keyed
 * by the parent thread a channel hangs off of, not any one participant
 * child thread. See docs/architecture/discussion-deliberation.md. */
export interface DiscussionMessageEvent {
  channelId: string;
  threadId: string;
  message: ChannelMessage;
}

// discussion:state's wire payload IS ChannelStatePayload — GetChannelState
// and the push event share the exact same shape (buildChannelState is the
// one projector behind both, Go-side).
export type DiscussionStateEvent = ChannelStatePayload;

/** Fetch page size for a full channel resync (initial load and gap
 * recovery both use this). If a channel ever fills exactly one page,
 * ChannelView.svelte logs a truncation warning instead of silently
 * capping history. */
export const DISCUSSION_CHANNEL_FETCH_LIMIT = 500;

export function applyDiscussionMessage(evt: DiscussionMessageEvent): void {
  if (!evt || !evt.threadId || !evt.message) return;
  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.applyChannelMessage(evt.message);
  }
}

export function applyDiscussionState(evt: DiscussionStateEvent): void {
  if (!evt || !evt.threadId || !evt.channelId) return;
  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.applyChannelState(evt);
  }
}

/**
 * Fetch a channel's FSM snapshot + a message page in one round trip,
 * without touching any particular pane. `afterSeq` is the EXCLUSIVE
 * cursor: message sequences are zero-based, so `-1` (not `0`) is
 * "fetch everything" — a `0` cursor silently excludes the channel's
 * very first message from a fresh load (the bug this replaces).
 *
 * Exported (rather than folded into `refreshDiscussionChannel` below)
 * so transport-gap recovery can fetch ONCE per channelId and apply the
 * identical result to every pane sharing that channel — mirroring how
 * `eventsTransportGap.ts`'s `provider:usage` case dedupes by threadId.
 * Per-pane `refreshDiscussionChannel` still owns its own cursor, since
 * ChannelView's initial load legitimately differs per pane (a second
 * pane opening the same channel later has already-loaded messages to
 * skip past).
 */
export async function fetchDiscussionChannelSnapshot(
  channelId: string,
  afterSeq = -1,
): Promise<{ state: ChannelStatePayload; messages: ChannelMessage[] }> {
  const [state, messages] = await Promise.all([
    GetChannelState(channelId),
    GetChannelMessages(channelId, afterSeq, DISCUSSION_CHANNEL_FETCH_LIMIT),
  ]);
  return {
    state: state as unknown as ChannelStatePayload,
    messages: (messages ?? []) as unknown as ChannelMessage[],
  };
}

/**
 * Resync one pane's discussion channel: GetChannelState for the FSM
 * snapshot, GetChannelMessages for the message page, applied straight
 * onto the pane. Used by ChannelView's initial load path and by
 * `eventsTransportGap.ts` (see there for why the gap-recovery case
 * doesn't call this directly for every affected pane). Returns the raw
 * fetched message page so a caller can detect a full page (possible
 * truncation) without duplicating the fetch-limit constant.
 *
 * `afterSeq` defaults to the pane's own highest-loaded sequence when it
 * already has messages, else `-1` (fetch everything) — the same
 * zero-based-sequence cursor rule `fetchDiscussionChannelSnapshot`
 * documents.
 */
export async function refreshDiscussionChannel(pane: ThreadPane): Promise<ChannelMessage[]> {
  const channelId = pane.thread?.discussionId;
  if (!channelId) return [];
  const loaded = pane.channelMessages;
  const afterSeq = loaded.length > 0 ? loaded[loaded.length - 1].sequence : -1;
  const { state, messages } = await fetchDiscussionChannelSnapshot(channelId, afterSeq);
  pane.applyChannelState(state);
  pane.applyChannelMessages(messages);
  return messages;
}
