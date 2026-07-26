// Per-pane discussion channel data layer. Owns the channel message log,
// the authoritative deliberation-FSM snapshot (status/turn counters/
// current speaker/roster), and the current speaker's live in-flight text
// tail. `thread.svelte.ts` creates one instance per pane and forwards its
// surface through `ThreadPane` getters/methods; `ChannelView.svelte`
// never touches this module directly (see State Boundaries in
// frontend/AGENTS.md).
import { nowForLiveContent } from './threadPaneShared';
import type {
  ChannelMessage,
  ChannelParticipantState,
  ChannelStatePayload,
} from '../types/discussion';
import {
  registerDiscussionLiveTail,
  unregisterDiscussionLiveTail,
  type DiscussionLiveTailHandler,
} from './discussionLiveTail';

export type ThreadChannelStatus = 'open' | 'concluded' | 'closed' | null;

export interface ThreadChannelLiveTail {
  readonly threadId: string;
  readonly itemId: string;
  readonly text: string;
}

export interface ThreadChannelState {
  readonly messages: ChannelMessage[];
  readonly status: ThreadChannelStatus;
  readonly turnCount: number;
  readonly maxTurns: number;
  readonly awaitingResponse: boolean;
  readonly currentSpeakerThreadId: string | null;
  readonly currentSpeakerRole: string | null;
  readonly participants: ChannelParticipantState[];
  readonly liveTail: ThreadChannelLiveTail | null;
  /** Non-reactive `performance.now()`-timebase stamp of the last live
   * channel advance (a genuinely new message, or live-tail growth).
   * Read imperatively by the scroll controller's `liveContentActive` —
   * see `utils/liveContentActivity.ts` and `pane.lastLiveContentAt`'s
   * identical rationale in `thread.svelte.ts`. Deliberately NOT
   * `$state`: tail deltas can stamp many times a second and nothing
   * here needs to reactively re-render off the stamp itself. */
  readonly lastLiveContentAt: number;
  /** Single-message merge for a live push (`discussion:message`) or the
   * message PostChannelMessage's own call returns. Dedupes/sorts by
   * sequence like `applyMessageBatch`; stamps `lastLiveContentAt` only
   * when the message is genuinely new. */
  applyMessage(message: ChannelMessage): void;
  /** Bulk merge for an initial load or gap-recovery resync page. Same
   * dedupe/sort semantics as `applyMessage`, applied once for the whole
   * batch instead of once per message. */
  applyMessageBatch(messages: ChannelMessage[]): void;
  /** Full deliberation-FSM snapshot apply — shared by the initial load
   * and every `discussion:state` push. Also owns the live-tail roster
   * registration (registers new participant thread ids, unregisters
   * removed ones) and drops a stale live tail whose thread is no longer
   * the awaited current speaker. */
  applyState(payload: ChannelStatePayload): void;
  /** Resets to the empty/default shape and unregisters every roster id
   * this instance holds in the live-tail registry — the pane calls this
   * on thread switch AND on pane destroy (`pane.clear()`), so this is
   * the one place that must not leak a registration. */
  clear(): void;
}

export function createThreadChannelState(): ThreadChannelState {
  let messages: ChannelMessage[] = $state([]);
  let status: ThreadChannelStatus = $state(null);
  let turnCount = $state(0);
  let maxTurns = $state(0);
  let awaitingResponse = $state(false);
  let currentSpeakerThreadId: string | null = $state(null);
  let currentSpeakerRole: string | null = $state(null);
  let participants: ChannelParticipantState[] = $state([]);
  let liveTail: ThreadChannelLiveTail | null = $state(null);
  let lastLiveContentAt = 0;
  let registeredRosterIds: Set<string> = new Set();

  function stampLiveContent(): void {
    lastLiveContentAt = nowForLiveContent();
  }

  /**
   * Merge channel messages into local state, de-duplicating by sequence.
   * Expected to be called with `afterSeq` set to the highest sequence
   * we've seen, so most calls append a small number of rows. Returns
   * whether any message was genuinely new (the array reference changes
   * only in that case) so callers can gate `stampLiveContent` on real
   * advances rather than duplicate/echoed pushes.
   */
  function mergeMessages(incoming: ChannelMessage[]): boolean {
    if (!incoming || incoming.length === 0) return false;

    const seenSequences = new Set(messages.map((message) => message.sequence));
    const additions: ChannelMessage[] = [];
    for (const message of incoming) {
      if (seenSequences.has(message.sequence)) continue;
      additions.push(message);
      seenSequences.add(message.sequence);
    }
    if (additions.length === 0) return false;
    const nextMessages = messages.concat(additions);
    nextMessages.sort((a, b) => a.sequence - b.sequence);
    messages = nextMessages;
    return true;
  }

  /**
   * An AGENT message landing means that participant's final text has
   * replaced whatever was streaming — drop the live tail for that
   * fromId (matches Go's `FromID: thread.ID` on the mirrored message;
   * see docs/architecture/discussion-deliberation.md).
   */
  function clearTailIfSuperseded(message: ChannelMessage): void {
    if (message.fromType !== 'agent') return;
    if (liveTail && liveTail.threadId === message.fromId) {
      liveTail = null;
    }
  }

  function applyMessage(message: ChannelMessage): void {
    if (!message) return;
    const added = mergeMessages([message]);
    clearTailIfSuperseded(message);
    if (added) stampLiveContent();
  }

  function applyMessageBatch(incoming: ChannelMessage[]): void {
    if (!incoming || incoming.length === 0) return;
    const added = mergeMessages(incoming);
    for (const message of incoming) clearTailIfSuperseded(message);
    if (added) stampLiveContent();
  }

  const tailHandler: DiscussionLiveTailHandler = {
    // Both handlers drop traffic for any thread that is not the
    // awaited current speaker. provider:item_event deltas batch up to
    // 50ms (eventsItemStream.ts) while discussion:message/state apply
    // instantly, so a queued delta/upsert for a participant whose turn
    // already posted can land AFTER applyState's stale-tail cleanup —
    // without this gate it would resurrect the cleared tail and
    // ChannelView would render the OLD speaker's fragment under the
    // NEW speaker's role label. Ordering makes the gate safe for the
    // next speaker: Go emits the claim's discussion:state (naming the
    // new currentSpeaker) BEFORE dispatching the turn prompt, so a
    // speaker's tail traffic always arrives after the state that
    // names them.
    applyTailUpsert(threadId, itemId, fullText) {
      if (threadId !== currentSpeakerThreadId) return;
      liveTail = { threadId, itemId, text: fullText };
      stampLiveContent();
    },
    applyTailDelta(threadId, itemId, chunk) {
      if (threadId !== currentSpeakerThreadId) return;
      if (!liveTail || liveTail.threadId !== threadId || liveTail.itemId !== itemId) {
        // No tail yet, or a new assistant_text item supersedes the
        // previous one — start fresh rather than append cross-item.
        liveTail = { threadId, itemId, text: chunk };
      } else {
        liveTail = { threadId, itemId, text: liveTail.text + chunk };
      }
      stampLiveContent();
    },
  };

  function applyState(payload: ChannelStatePayload): void {
    if (!payload) return;
    status = (payload.status || null) as ThreadChannelStatus;
    turnCount = payload.turnCount;
    maxTurns = payload.maxTurns;
    awaitingResponse = payload.awaitingResponse;
    currentSpeakerThreadId = payload.currentSpeakerThreadId || null;
    currentSpeakerRole = payload.currentSpeakerRole || null;
    participants = payload.participants ?? [];

    const nextRosterIds = new Set(
      participants.map((p) => p.threadId).filter((id): id is string => !!id),
    );
    for (const id of nextRosterIds) {
      if (!registeredRosterIds.has(id)) registerDiscussionLiveTail(id, tailHandler);
    }
    for (const id of registeredRosterIds) {
      if (!nextRosterIds.has(id)) unregisterDiscussionLiveTail(id, tailHandler);
    }
    registeredRosterIds = nextRosterIds;

    // Stale-tail cleanup: a tool-only turn advances CurrentSpeaker
    // without ever posting an agent message, so `clearTailIfSuperseded`
    // never fires for the old speaker. Once state says someone else is
    // now awaited, any leftover tail is for a turn that already ended.
    if (liveTail && liveTail.threadId !== currentSpeakerThreadId) {
      liveTail = null;
    }
  }

  function clear(): void {
    messages = [];
    status = null;
    turnCount = 0;
    maxTurns = 0;
    awaitingResponse = false;
    currentSpeakerThreadId = null;
    currentSpeakerRole = null;
    participants = [];
    liveTail = null;
    lastLiveContentAt = 0;
    for (const id of registeredRosterIds) unregisterDiscussionLiveTail(id, tailHandler);
    registeredRosterIds = new Set();
  }

  return {
    get messages() { return messages; },
    get status() { return status; },
    get turnCount() { return turnCount; },
    get maxTurns() { return maxTurns; },
    get awaitingResponse() { return awaitingResponse; },
    get currentSpeakerThreadId() { return currentSpeakerThreadId; },
    get currentSpeakerRole() { return currentSpeakerRole; },
    get participants() { return participants; },
    get liveTail() { return liveTail; },
    get lastLiveContentAt() { return lastLiveContentAt; },
    applyMessage,
    applyMessageBatch,
    applyState,
    clear,
  };
}
