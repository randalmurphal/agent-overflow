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
//   - eventsMessageRevert.ts — user-message revert (Stop/Esc un-send)
//   - eventsTransportGap.ts  — missed-seq resync
//   - eventsDiscussion.ts    — discussion:message / discussion:state push
//   - eventsNotification.ts  — OS activation routing + cold-start queue
//   - eventsHighlight.ts     — highlight:seed span ingest (remote clients)
//   - eventsWorktreeSetup.ts — worktree:setup run stream + snapshot resync
//   - eventsSessionImport.ts — session-import:progress run frames (+ the
//                              transport-loss end condition a run has no
//                              other way to learn about)
//
// This file itself stays a thin fan-in: channel names, generics, and the
// teardown order live here; the reaction logic lives in the domain modules.
import type {
  ApprovalEvent,
  ItemStreamEvent,
  ModelFallbackEvent,
  ProviderAccountEvent,
  ProviderAccountUsageErrorEvent,
  ProviderSessionAccountEvent,
  SystemStatsEvent,
  TodoUpdateEvent,
  ProviderStatusEvent,
  SessionDiedEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
  WorktreeSetupEvent,
} from '../types/events';
import type {
  TerminalExitEventPayload,
  TerminalOutputEventPayload,
} from '../types/terminal';
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import { setSystemStats } from './systemStats.svelte';
import { transportGapChannel } from '../transport/wsClient';
// wailsEventOn lives in a leaf module so low-level stores can subscribe to
// backend events without importing this handler module; imported here for
// setupEventListeners() use and re-exported below for existing import sites.
import { wailsEventOn } from './wailsEvents';
import {
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
  hydrateProviderAccounts,
  applyProviderSessionAccount,
  applyTurnStarted,
  applyTurnCompleted,
  applySessionDied,
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
  applyCommandLifecycle,
  type QueueStateChangedPayload,
  type QueueFlushedPayload,
  type QueueRestoredPayload,
  type CommandLifecyclePayload,
} from './eventsQueue';
import {
  applyFastModeState,
  type FastModeStatePayload,
} from './fastModeState.svelte';
import {
  applyCompactingState,
  type CompactingStatePayload,
} from './compactingState.svelte';
import { applySubagentProgress } from './subagentProgress.svelte';
import type { SubagentProgressEvent } from '../types/events';
import {
  applyProviderCommands,
  type ProviderCommandsPayload,
} from './providerCommands.svelte';
import { bumpUsageRefresh } from './usageRefresh.svelte';
import { applyUserMessageReverted } from './eventsMessageRevert';
import { applyTransportGap } from './eventsTransportGap';
import { applyWorktreeSetup } from './eventsWorktreeSetup';
import {
  applyThreadTitleGeneration,
  type ThreadTitleGenerationEvent,
} from './threadTitleGeneration.svelte';
import {
  applyDiscussionMessage,
  applyDiscussionState,
  type DiscussionMessageEvent,
  type DiscussionStateEvent,
} from './eventsDiscussion';
import { applyPRReviewUpdated } from './eventsPRReview';
import { setupSessionImportEvents } from './eventsSessionImport';
import {
  applyHighlightSeed,
  applyHighlightDiffSeed,
  type HighlightSeedEvent,
  type HighlightDiffSeedEvent,
} from './eventsHighlight';
import { clearAllDiscussionLiveTail } from './discussionLiveTail';
import { hydrateRateLimitsSnapshots } from './eventsRateLimits';
import {
  applyNotificationActivated,
} from './eventsNotification';
import { parseNotificationTarget } from './notificationActivationQueue';
import type {
  WorkflowEngineStateEvent,
  WorkflowErrorEvent,
  WorkflowItemStateEvent,
  WorkflowPhaseStateEvent,
  WorkflowSoftStopEvent,
} from '../types/workflow';
import {
  applyWorkflowDefinitionsChangedEvent,
  applyWorkflowEngineStateEvent,
  applyWorkflowErrorEvent,
  applyWorkflowItemStateEvent,
  applyWorkflowPhaseStateEvent,
  applyWorkflowSoftStopEvent,
} from './eventsWorkflow';
import { addToast } from './toast.svelte';

/**
 * Set up the app's Wails event listeners.
 * Returns a cleanup function that removes all listeners.
 */
export function setupEventListeners(): () => void {
  resetItemEventQueue();

  const cancelApproval = wailsEventOn<ApprovalEvent>('provider:approval', applyApprovalEvent);
  const cancelNotificationActivated = wailsEventOn<unknown>(
    'notification:activated',
    (value) => {
      const target = parseNotificationTarget(value);
      if (target) applyNotificationActivated(target);
      else console.warn('notification:activated: invalid target', value);
    },
  );
  const cancelUserInput = wailsEventOn<UserInputEvent>('provider:user_input', applyUserInputEvent);

  const cancelUsage = wailsEventOn<UsageEvent>('provider:usage', applyUsageEvent);
  // Startup probes can finish before a first websocket connection has any
  // provider:usage sequence to replay. Install the live listener first, then
  // hydrate the backend's retained last-known account snapshots.
  void hydrateRateLimitsSnapshots().catch((error) => {
    console.warn('events: hydrate rate-limit snapshots failed', error);
  });
  const cancelModelFallback = wailsEventOn<ModelFallbackEvent>(
    'provider:model_fallback',
    applyModelFallback,
  );

  const cancelProviderStatus = wailsEventOn<ProviderStatusEvent>('provider:status', applyProviderStatus);

  // provider:account — account probe result on cache miss, plus login /
  // switch / removal transitions. Hydrates the global accountInfo store; the
  // rate-limit ring popover reads it for the "Plan: <planType>" line. The
  // startup probe can complete before this listener exists (its cache then
  // suppresses re-emits for the process lifetime), so after installing the
  // live listener, pull the current selection the same way the rate-limit
  // snapshots are pulled above.
  const cancelProviderAccount = wailsEventOn<ProviderAccountEvent>(
    'provider:account',
    applyProviderAccount,
  );
  void hydrateProviderAccounts().catch((error) => {
    console.warn('events: hydrate provider accounts failed', error);
  });
  const cancelProviderSessionAccount = wailsEventOn<ProviderSessionAccountEvent>(
    'provider:session_account',
    applyProviderSessionAccount,
  );
  const cancelProviderAccountUsageError = wailsEventOn<ProviderAccountUsageErrorEvent>(
    'provider:account_usage_error',
    (evt) => {
      if (!evt?.provider || !evt.accountId) return;
      const label = evt.provider === 'claude' ? 'Claude' : 'Codex';
      addToast('warning', evt.message || `${label} connected, but usage could not be refreshed.`);
    },
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
  // usage:thread_cost — a PROVIDER's own cost figure for a thread landed
  // after the turn that produced it had already settled (Codex asks its
  // backend asynchronously; see app_codex_thread_cost.go). The turn-completed
  // bump has already fired by then, so without this the chip would keep
  // showing the rate-table estimate until the next turn. Payload is the
  // thread id only: every usage surface refetches from the backend.
  const cancelThreadCost = wailsEventOn<{ threadId?: string }>('usage:thread_cost', (payload) => {
    const threadId = payload?.threadId;
    if (threadId) bumpUsageRefresh(threadId);
  });
  // provider:session_died — provider subprocess exited mid-turn. Drives
  // the per-pane Reconnect banner (separately from the synthesized
  // turn-completed event that clears the working indicator). The
  // historical trace lives in the timeline as a `notification` row.
  const cancelSessionDied = wailsEventOn<SessionDiedEvent>('provider:session_died', applySessionDied);
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
  // provider:command_lifecycle — Claude's per-message delivery ack for a
  // user message written to provider stdin, keyed onto the Zone 2 entry
  // the backend resolved it to. Purely additive labelling: older CLIs
  // emit nothing here and the queue events alone still drive the UI.
  const cancelCommandLifecycle = wailsEventOn<CommandLifecyclePayload>(
    'provider:command_lifecycle',
    applyCommandLifecycle,
  );

  // provider:fast_mode — the provider's own live report of whether fast
  // mode is actually serving this thread's turns. Restated on every
  // session init and every turn-complete, so the newest frame is the
  // whole answer; nothing is persisted on either side.
  const cancelFastModeState = wailsEventOn<FastModeStatePayload>(
    'provider:fast_mode',
    applyFastModeState,
  );

  // provider:compacting — the provider is summarizing this thread's context
  // right now. Swaps the activity rail's "Working" label to "Compacting" for
  // the duration; the window can span minutes of wire silence, so refresh
  // re-learns it from GetThreadLiveState rather than from a frame.
  const cancelCompactingState = wailsEventOn<CompactingStatePayload>(
    'provider:compacting',
    applyCompactingState,
  );

  // provider:subagent_progress — the latest live counters of one running
  // subagent (tool count, tokens, elapsed, activity line). Triage merges
  // each tick over the previous one, so the newest frame is the whole
  // answer; the final numbers persist on the launch row at its terminal.
  const cancelSubagentProgress = wailsEventOn<SubagentProgressEvent>(
    'provider:subagent_progress',
    applySubagentProgress,
  );

  // provider:commands — the CLI's own list of slash commands it will execute
  // without an API call. Restated wholesale on every session init and every
  // `commands_changed` push, so the newest frame REPLACES the previous one;
  // never merge two frames. Absence of any frame is "unknown", which the
  // composer's menu renders differently from "this session has none".
  const cancelProviderCommands = wailsEventOn<ProviderCommandsPayload>(
    'provider:commands',
    applyProviderCommands,
  );

  const cancelUserMessageReverted = wailsEventOn<UserMessageRevertedEvent | null>(
    'user_message:reverted',
    applyUserMessageReverted,
  );

  const cancelThreadUpdated = wailsEventOn<ThreadUpdateEvent>('thread:updated', applyThreadUpdated);

  // thread:title_generation — the completion frame of one title-generation
  // run (auto first-turn, heal, or user-triggered regeneration). Clears the
  // pending flag the regenerate affordance renders from; the redacted error
  // is surfaced only when a user-triggered regeneration was awaiting it.
  const cancelThreadTitleGeneration = wailsEventOn<ThreadTitleGenerationEvent | null>(
    'thread:title_generation',
    applyThreadTitleGeneration,
  );

  // worktree:setup — the per-project setup recipe streaming over a worktree a
  // chat thread just had cut. Its own channel because only the setup panel
  // consumes it and its frames carry local command output (transport keeps it
  // loopback-only). GetThreadWorktreeSetup is the reconnect companion; see
  // stores/worktreeSetup.svelte.ts for the sequence/hydration contract.
  const cancelWorktreeSetup = wailsEventOn<WorktreeSetupEvent>(
    'worktree:setup',
    applyWorktreeSetup,
  );

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
  // workflow:* — the typed run-record channel. Item/phase transitions keep
  // the overlay's run cache live (and the sidebar's needs-attention badge
  // authoritative) without polling; engine-state carries the one global pause
  // flag; definitions-changed fires when the studio save path writes a
  // definition file.
  const cancelWorkflowError = wailsEventOn<WorkflowErrorEvent>(
    'workflow:error', applyWorkflowErrorEvent,
  );
  const cancelWorkflowItemState = wailsEventOn<WorkflowItemStateEvent>(
    'workflow:item-state', applyWorkflowItemStateEvent,
  );
  const cancelWorkflowPhaseState = wailsEventOn<WorkflowPhaseStateEvent>(
    'workflow:phase-state', applyWorkflowPhaseStateEvent,
  );
  const cancelWorkflowEngineState = wailsEventOn<WorkflowEngineStateEvent>(
    'workflow:engine-state', applyWorkflowEngineStateEvent,
  );
  // soft-stop is its own channel because it is not a transition: the run keeps
  // running, it has just been asked to stop at its next call boundary (D36).
  const cancelWorkflowSoftStop = wailsEventOn<WorkflowSoftStopEvent>(
    'workflow:soft-stop', applyWorkflowSoftStopEvent,
  );
  const cancelWorkflowDefinitions = wailsEventOn(
    'workflow:definitions-changed', applyWorkflowDefinitionsChangedEvent,
  );

  // session-import:progress — one frame per session an import run finishes
  // plus exactly one terminal frame. Its own module because consuming the
  // channel also means watching the connection: a dropped transport is the
  // run's other terminal condition and nothing replays the frames it ate.
  const cancelSessionImport = setupSessionImportEvents();

  // highlight:seed — backend-pushed syntax spans for streaming code
  // fences. Remote-only by transport filtering; loopback clients never
  // see this channel (they highlight via the local RPC round trip).
  const cancelHighlightSeed = wailsEventOn<HighlightSeedEvent>(
    'highlight:seed',
    applyHighlightSeed,
  );
  const cancelHighlightDiffSeed = wailsEventOn<HighlightDiffSeedEvent>(
    'highlight:diff_seed',
    applyHighlightDiffSeed,
  );

  return () => {
    cancelItemEvent();
    flushItemEventQueue();
    cancelApproval();
    cancelNotificationActivated();
    cancelUserInput();
    cancelUsage();
    cancelModelFallback();
    cancelProviderStatus();
    cancelProviderAccount();
    cancelProviderSessionAccount();
    cancelProviderAccountUsageError();
    cancelSystemStats();
    cancelTurnStarted();
    cancelTurnCompleted();
    cancelThreadCost();
    cancelSessionDied();
    cancelTodoUpdate();
    cancelTerminalOutput();
    cancelTerminalExit();
    cancelQueueStateChanged();
    cancelQueueFlushed();
    cancelQueueRestored();
    cancelCommandLifecycle();
    cancelFastModeState();
    cancelCompactingState();
    cancelSubagentProgress();
    cancelProviderCommands();
    cancelUserMessageReverted();
    cancelThreadUpdated();
    cancelThreadTitleGeneration();
    cancelWorktreeSetup();
    cancelDefaultSwapped();
    cancelTransportGap();
    cancelDesignReloadMain();
    cancelDesignOptionsUpdate();
    cancelModeChanged();
    cancelRuntimeModeChanged();
    cancelDiscussionMessage();
    cancelDiscussionState();
    cancelPRUpdated();
    cancelWorkflowError();
    cancelWorkflowItemState();
    cancelWorkflowPhaseState();
    cancelWorkflowEngineState();
    cancelWorkflowSoftStop();
    cancelWorkflowDefinitions();
    cancelSessionImport();
    cancelHighlightSeed();
    cancelHighlightDiffSeed();
    clearAllDesignThrottles();
    clearAllDiscussionLiveTail();
  };
}
