// Composition root for backend event wiring. `setupEventListeners()` is the
// single place that subscribes to every Wails channel and fans events out
// into the domain modules that actually own the reaction:
//
//   - eventsThreadRows.ts    — cached Thread row projections (shared leaf)
//   - eventsItemStream.ts    — provider:item_event batching/upsert dispatch,
//                              incl. the discussion live-tail side-channel
//                              feed (assistant_text from unmounted
//                              participant threads → discussionLiveTail.ts)
//   - eventsProvider.ts      — approvals, usage, turn/session lifecycle
//   - eventsDesign.ts        — design preview/options throttled reload
//   - eventsTerminal.ts      — backgrounded-terminal output/exit
//   - eventsQueue.ts         — send-queue mirror (state/flushed/restored)
//   - eventsCheckpoint.ts    — checkpoint capture/revert + message revert
//   - eventsTransportGap.ts  — missed-seq resync
//   - eventsDiscussion.ts    — discussion:message / discussion:state push
//
// This file itself stays a thin fan-in: channel names, generics, and the
// teardown order live here; the reaction logic lives in the domain modules.
import type {
  ApprovalEvent,
  ItemStreamEvent,
  ModelFallbackEvent,
  ProviderAccountEvent,
  SystemStatsEvent,
  TodoUpdateEvent,
  ProviderStatusEvent,
  SessionDiedEvent,
  SubagentNotificationEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
} from '../types/events';
import type {
  TerminalExitEventPayload,
  TerminalOutputEventPayload,
} from '../types/terminal';
import type {
  CheckpointCapturedEvent,
  CheckpointErrorEvent,
  CheckpointRevertedEvent,
  CheckpointUnavailableEvent,
  UserMessageRevertedEvent,
} from '../types/checkpoint';
import { setSystemStats } from './systemStats.svelte';
import { transportGapChannel } from '../transport/wsClient';
// wailsEventOn lives in a leaf module so low-level stores can subscribe to
// backend events without importing this handler module; imported here for
// setupEventListeners() use and re-exported below for existing import sites.
import { wailsEventOn } from './wailsEvents';
import {
  DESIGN_RELOAD_MAIN_EVENT,
  DESIGN_OPTIONS_UPDATE_EVENT,
} from './eventNames';
import {
  onItemUpsert,
  applyItemStreamEvent,
  flushItemEventQueue,
  resetItemEventQueue,
} from './eventsItemStream';
import {
  applyThreadUpdated,
  type ThreadUpdateEvent,
  applyModeChanged,
  type ModeChangedPayload,
  applyRuntimeModeChanged,
  type RuntimeModeChangedPayload,
} from './eventsThreadRows';
import {
  applyApprovalEvent,
  applyUserInputEvent,
  applyUsageEvent,
  applyProviderStatus,
  applyProviderAccount,
  applyTurnStarted,
  applyTurnCompleted,
  applySessionDied,
  applySubagentNotification,
  applyTodoUpdate,
  applyDefaultSwapped,
  applyModelFallback,
  type DefaultSwappedPayload,
} from './eventsProvider';
import {
  handleDesignReloadMain,
  type DesignReloadMainPayload,
  applyDesignOptionsUpdate,
  type DesignOptionsUpdatePayload,
  clearAllDesignThrottles,
} from './eventsDesign';
import { applyTerminalOutput, applyTerminalExit } from './eventsTerminal';
import {
  applyQueueStateChanged,
  applyQueueFlushed,
  applyQueueRestored,
  type QueueStateChangedPayload,
  type QueueFlushedPayload,
  type QueueRestoredPayload,
} from './eventsQueue';
import {
  applyCheckpointCaptured,
  applyCheckpointUnavailable,
  applyCheckpointError,
  applyCheckpointReverted,
  applyUserMessageReverted,
} from './eventsCheckpoint';
import { applyTransportGap } from './eventsTransportGap';
import {
  applyDiscussionMessage,
  applyDiscussionState,
  type DiscussionMessageEvent,
  type DiscussionStateEvent,
} from './eventsDiscussion';
import { applyPRReviewUpdated } from './eventsPRReview';
import { clearAllDiscussionLiveTail } from './discussionLiveTail';

/**
 * Frontend custom DOM event names live in `./eventNames` so consumers
 * that this file depends on transitively (notably panes.svelte.ts) can
 * import them without forming an import cycle. Re-exported here so
 * existing consumers that pull names from `./events` keep working —
 * new code should prefer the direct `./eventNames` import.
 */
export {
  DESIGN_RELOAD_MAIN_EVENT,
  DESIGN_OPTIONS_UPDATE_EVENT,
  PICKER_TOGGLE_INPUT_EVENT,
  RENAME_THREAD_EVENT,
  OPEN_SHIP_CHANGES_EVENT,
  OPEN_SETTINGS_EVENT,
  REVEAL_PANE_EVENT,
} from './eventNames';

// wailsEventOn is defined in ./wailsEvents (a leaf module) and re-exported here
// so existing subscribers (terminal drawer, diff panel, mcpServers, …) keep
// importing it from './events'. It lives in a leaf so low-level stores can
// subscribe without importing this handler module — see wailsEvents.ts.
export { wailsEventOn };

// onItemUpsert is defined in ./eventsItemStream (the item-batching leaf) and
// re-exported here so existing subscribers (activityRailBackground,
// workspaceChangeLock, proposedPlans) keep importing it from './events'.
export { onItemUpsert };

/**
 * Set up the app's Wails event listeners.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  resetItemEventQueue();

  const cancelApproval = wailsEventOn<ApprovalEvent>('provider:approval', applyApprovalEvent);
  const cancelUserInput = wailsEventOn<UserInputEvent>('provider:user_input', applyUserInputEvent);

  const cancelUsage = wailsEventOn<UsageEvent>('provider:usage', applyUsageEvent);
  const cancelModelFallback = wailsEventOn<ModelFallbackEvent>(
    'provider:model_fallback',
    applyModelFallback,
  );

  const cancelProviderStatus = wailsEventOn<ProviderStatusEvent>('provider:status', applyProviderStatus);

  // provider:account — startup probe result (one event per provider).
  // Hydrates the global accountInfo store; the rate-limit ring popover
  // reads it for the "Plan: <planType>" line.
  const cancelProviderAccount = wailsEventOn<ProviderAccountEvent>(
    'provider:account',
    applyProviderAccount,
  );

  // system:stats — periodic host CPU + memory snapshot (~2s cadence)
  // driving the sidebar SystemStatsFooter. Coarse aggregate values,
  // no per-thread or per-process detail. Validate every field —
  // anything coming over the WS could in principle be malformed, and
  // partial-shape acceptance would let NaN/undefined propagate into
  // the sidebar render.
  const cancelSystemStats = wailsEventOn<SystemStatsEvent>(
    'system:stats',
    (evt) => {
      if (
        !evt
        || typeof evt.isWsl !== 'boolean'
        || typeof evt.cpuPercent !== 'number'
        || typeof evt.memUsedBytes !== 'number'
        || typeof evt.memTotalBytes !== 'number'
      ) {
        return;
      }
      setSystemStats(evt);
    },
  );

  // provider:item_event is the canonical ordered timeline mutation stream.
  // Upserts and deltas intentionally share one Wails channel so streaming
  // text cannot race lifecycle snapshots across separate event names.
  const cancelItemEvent = wailsEventOn<ItemStreamEvent>('provider:item_event', applyItemStreamEvent);

  // provider:turn_{started,completed} — wire-pushed turn lifecycle.
  // These are the sole drivers of the global active-turn registry
  // (threadStatuses.svelte.ts → getActiveTurn) and
  // `pane.latestSettledTurn`. See invariant 22 and
  // docs/architecture/turn-lifecycle.md §Frontend state shape.
  const cancelTurnStarted = wailsEventOn<TurnStartedEvent>('provider:turn_started', applyTurnStarted);
  const cancelTurnCompleted = wailsEventOn<TurnCompletedEvent>('provider:turn_completed', applyTurnCompleted);
  // provider:session_died — provider subprocess exited mid-turn. Drives
  // the per-pane Reconnect banner (separately from the synthesized
  // turn-completed event that clears the working indicator). The
  // historical trace lives in the timeline as a `notification` row.
  const cancelSessionDied = wailsEventOn<SessionDiedEvent>('provider:session_died', applySessionDied);
  // provider:subagent_notification — Codex passes subagent metadata
  // through; no UI renders this yet, but the pane records it so future
  // surfaces can subscribe without re-wiring.
  const cancelSubagentNotification = wailsEventOn<SubagentNotificationEvent>(
    'provider:subagent_notification',
    applySubagentNotification,
  );
  // provider:todo_update — Claude TodoWrite + Codex update_plan funnel
  // through here after parser normalisation. Drives the activity
  // rail's Todos segment. Has zero timeline footprint by design (see
  // ActivityRail.svelte).
  const cancelTodoUpdate = wailsEventOn<TodoUpdateEvent>(
    'provider:todo_update',
    applyTodoUpdate,
  );
  const cancelTerminalOutput = wailsEventOn<TerminalOutputEventPayload>(
    'terminal:output',
    applyTerminalOutput,
  );
  const cancelTerminalExit = wailsEventOn<TerminalExitEventPayload>(
    'terminal:exit',
    applyTerminalExit,
  );

  // provider:queue_state_changed — backend per-thread queue snapshot.
  // Authoritative replacement of the frontend's Zone 1 mirror;
  // arrives on RegisterQueueItem and after the flush trigger drains the
  // batch. provider:queue_flushed follows successful provider writes, so
  // failed items never enter the sent-but-unconfirmed pending list.
  const cancelQueueStateChanged = wailsEventOn<QueueStateChangedPayload>(
    'provider:queue_state_changed',
    applyQueueStateChanged,
  );
  const cancelQueueFlushed = wailsEventOn<QueueFlushedPayload>(
    'provider:queue_flushed',
    applyQueueFlushed,
  );
  const cancelQueueRestored = wailsEventOn<QueueRestoredPayload>(
    'provider:queue_restored',
    applyQueueRestored,
  );

  const cancelCheckpointCaptured = wailsEventOn<CheckpointCapturedEvent | null>(
    'checkpoint:captured',
    applyCheckpointCaptured,
  );
  const cancelCheckpointUnavailable = wailsEventOn<CheckpointUnavailableEvent | null>(
    'checkpoint:unavailable',
    applyCheckpointUnavailable,
  );
  const cancelCheckpointError = wailsEventOn<CheckpointErrorEvent | null>(
    'checkpoint:error',
    applyCheckpointError,
  );
  const cancelCheckpointReverted = wailsEventOn<CheckpointRevertedEvent | null>(
    'checkpoint:reverted',
    applyCheckpointReverted,
  );
  const cancelUserMessageReverted = wailsEventOn<UserMessageRevertedEvent | null>(
    'user_message:reverted',
    applyUserMessageReverted,
  );

  const cancelThreadUpdated = wailsEventOn<ThreadUpdateEvent>('thread:updated', applyThreadUpdated);

  // provider:default_swapped — backend auto-flipped the default
  // provider because the saved one was not_found and the other was
  // ready. Surface a toast so the user notices the change before they
  // wonder why the next thread routed to a different CLI; the value
  // can still be reverted manually in Settings.
  const cancelDefaultSwapped = wailsEventOn<DefaultSwappedPayload>(
    'provider:default_swapped',
    applyDefaultSwapped,
  );

  // transport:gap — synthetic event fired by wsClient.ts when the
  // server reports a missed seq on a channel. Coarse-grained recovery:
  // re-fetch the active pane's window so SQLite (the authoritative
  // history cache) backfills whatever was lost. We don't try to be
  // surgical because the gap signal doesn't carry the missed range.
  //
  // The handler matches on the channel name we lost rather than each
  // payload kind because a single gap on `provider:item_event` can
  // straddle upserts AND deltas; refreshing the whole pane is the
  // simplest correct response.
  const cancelTransportGap = wailsEventOn<{ channel: string; seq: number }>(
    transportGapChannel,
    applyTransportGap,
  );

  // design:reload-main — file watcher fired in the thread's main/
  // directory. The preview panel listens for the throttled DOM event we
  // re-dispatch and bumps its cache-bust counter. Throttling lives here
  // (not in the panel) so a rapid burst of saves only causes one
  // iframe reload per 500ms across all consumers.
  const cancelDesignReloadMain = wailsEventOn<DesignReloadMainPayload>(
    'design:reload-main',
    handleDesignReloadMain,
  );

  // design:options-update — agent rewrote files in options/{setId}/ for
  // the thread. Hydrates `pane.activeOptionSet` for the matching pane
  // (so the N-up grid renders) and forwards a DOM event for any future
  // component that needs the raw signal. Throttled per-thread so a
  // burst of file writes doesn't fan out a list-options RPC for each.
  const cancelDesignOptionsUpdate = wailsEventOn<DesignOptionsUpdatePayload>(
    'design:options-update',
    applyDesignOptionsUpdate,
  );

  // thread:runtime_mode_changed — backend persisted a new three-tier
  // approval mode. Refresh the sidebar cache and active pane; AccessToggle
  // only stages draft intent, so this event or SendMessageWithOptions'
  // returned Thread is what makes persisted runtime state visible.
  const cancelRuntimeModeChanged = wailsEventOn<RuntimeModeChangedPayload>(
    'thread:runtime_mode_changed',
    applyRuntimeModeChanged,
  );

  // thread:mode_changed — the backend persisted a new mode. We update the
  // cached thread row (so sidebar badges refresh) and, when the change
  // landed on an active session, surface a toast prompting the user to
  // reconnect so the session can pick up the new mode's config.
  const cancelModeChanged = wailsEventOn<ModeChangedPayload>(
    'thread:mode_changed',
    applyModeChanged,
  );

  // discussion:message / discussion:state — push-driven replacement for
  // ChannelView's old 2.5s poll. Routed to every pane whose threadId
  // matches the event's PARENT thread id (a discussion channel hangs
  // off the parent, not any one participant child thread). See
  // eventsDiscussion.ts and docs/architecture/discussion-deliberation.md.
  const cancelDiscussionMessage = wailsEventOn<DiscussionMessageEvent>(
    'discussion:message',
    applyDiscussionMessage,
  );
  const cancelDiscussionState = wailsEventOn<DiscussionStateEvent>(
    'discussion:state',
    applyDiscussionState,
  );
  const cancelPRUpdated = wailsEventOn('pr:updated', applyPRReviewUpdated);

  return () => {
    cancelItemEvent();
    flushItemEventQueue();
    cancelApproval();
    cancelUserInput();
    cancelUsage();
    cancelModelFallback();
    cancelProviderStatus();
    cancelProviderAccount();
    cancelSystemStats();
    cancelTurnStarted();
    cancelTurnCompleted();
    cancelSessionDied();
    cancelSubagentNotification();
    cancelTodoUpdate();
    cancelTerminalOutput();
    cancelTerminalExit();
    cancelQueueStateChanged();
    cancelQueueFlushed();
    cancelQueueRestored();
    cancelCheckpointCaptured();
    cancelCheckpointUnavailable();
    cancelCheckpointError();
    cancelCheckpointReverted();
    cancelUserMessageReverted();
    cancelThreadUpdated();
    cancelDefaultSwapped();
    cancelTransportGap();
    cancelDesignReloadMain();
    cancelDesignOptionsUpdate();
    cancelModeChanged();
    cancelRuntimeModeChanged();
    cancelDiscussionMessage();
    cancelDiscussionState();
    cancelPRUpdated();
    clearAllDesignThrottles();
    clearAllDiscussionLiveTail();
  };
}
