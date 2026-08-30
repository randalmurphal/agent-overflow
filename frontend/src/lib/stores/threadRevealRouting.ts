// stores/threadRevealRouting.ts
//
// OWNS what happens to one row's text as it reveals: smoother construction
// (including the settings-driven `revealImmediately` snap), the per-frame
// `onReveal` transaction that routes each delta down the DIRECT sink path or
// the AUTHORITATIVE row-write path, the reasoning-tail trim and its live
// payload append, and the patch decision tree (snap statuses, extend vs
// overwrite, caught-up terminal settle, bare-status settle).
//
// MUST NOT own resources or the gate. The smoother map, the retained tails
// and the sink registry belong to `threadRevealSmoothers.ts`; `revealBoundary`
// and every "mutate then re-derive" transaction belong to
// `threadRevealGate.svelte.ts`. This module calls both and stores nothing of
// its own. It also must not decide the text of a WHOLESALE replacement —
// that is `prepareItemReplacement` in `threadStreamingReveal.svelte.ts`, the
// single chokepoint the reveal invariant is asserted at.

import type { Item, ItemKind } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import { PerItemSmoother } from '../markdown/smoothing/PerItemSmoother';
import {
  THINKING_TAIL_RUNES,
  getSmoothingClockForTest,
  isReasoningTailKind,
  trimToTailRunes,
} from './threadPaneShared';
import { getSettings } from './settings.svelte';
import {
  COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  thinkingPayloadVersionForItem,
} from '../utils/payloadVersion';
import type { ProvenAppend } from '../markdown';
import type { RevealGate } from './threadRevealGate.svelte';
import {
  absorbReceivedSuffix,
  isSnapStatus,
  summaryRepresentsReceived,
  throwCollectedErrors,
  type ItemSmoothing,
  type RevealSmootherRegistry,
} from './threadRevealSmoothers';

export interface RevealRoutingOptions {
  registry: RevealSmootherRegistry;
  gate: RevealGate;
  /** Current item for an id, or undefined when not loaded. */
  getItemById(itemId: string): Item | undefined;
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The current item window, sorted by (turnIndex, itemIndex). */
  getItems(): Item[];
  /** Reactive write-through of one row (pane does `items[index] = item`). */
  setItemAt(index: number, item: Item): void;
  /** Commit a preflighted literal suffix without waking Svelte. */
  appendDirectAssistantLiteral(
    index: number,
    itemId: string,
    append: ProvenAppend,
    updatedAt: number,
  ): void;
  /** Stamp the live-content latch (pane's stampLiveContent). */
  stampLiveContent(): void;
  /** rowUiState.appendLivePayloadDeltaForItem — live reasoning-tail payload append. */
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    delta: string,
    payloadVersion?: unknown,
    previousLiveTail?: string,
  ): void;
}

export interface RevealRouting {
  appendStreamingDelta(
    itemId: string,
    currentSummary: string,
    delta: string,
    updatedAt: number,
  ): void;
  applyPatch(itemId: string, patch: ItemPatchEvent['patch']): void;
}

export function createRevealRouting(options: RevealRoutingOptions): RevealRouting {
  const { registry, gate } = options;
  const itemSmoothers = registry.smoothers;
  const assistantReveal = registry.assistantReveal;

  // The payload-expansion namespace a reasoning-tail row reads from, matched by
  // the row component so a mid-stream live delta lands where an expand will
  // read it.
  function reasoningExpansionStateKey(kind: ItemKind | string): string {
    return kind === 'compaction_reasoning'
      ? COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY
      : THINKING_PAYLOAD_EXPANSION_STATE_KEY;
  }

  function runRevealTransaction(itemId: string, reveal: () => void): void {
    try {
      reveal();
      return;
    } catch (failure) {
      // PerItemSmoother advances its cursor before invoking this callback. A
      // failed row write, payload update, or sink transition cannot be retried
      // on another frame. Drop the now-unusable smoother and re-derive the
      // gate before surfacing the failure, or one bad row permanently
      // withholds every successor behind its stale frontier.
      const errors: unknown[] = [failure];
      if (itemSmoothers.has(itemId)) {
        try {
          registry.disposeSmootherState(itemId);
        } catch (error) {
          errors.push(error);
        }
      }
      try {
        gate.recomputeReveal();
      } catch (error) {
        errors.push(error);
      }
      throwCollectedErrors(
        errors,
        `streaming reveal callback recovery failed for ${itemId}`,
      );
    }
  }

  function getOrCreateSmoothing(
    itemId: string,
    initialReceived: string,
  ): ItemSmoothing {
    const existing = itemSmoothers.get(itemId);
    if (existing) return existing;

    const seeded = registry.seedFromRetainedTail(itemId, initialReceived);

    // Closure state for this item's smoother. Updated by each delta
    // and read inside `onReveal` so the row's `updatedAt` stays close
    // to wire time even as the smoother lags.
    let latestUpdatedAt = 0;
    // Full previous revealed text. Appending each emitted delta here keeps a
    // canonical cons string without asking the smoother to join its whole
    // received buffer. Reasoning also passes the previous value into its live
    // payload expansion so that view stays on the same cursor.
    let previousRevealed = seeded;

    const smoother = new PerItemSmoother({
      initialReceived: seeded,
      // Reveal the whole received backlog per wire chunk (one mutation
      // per chunk, a few Hz) instead of the animated 48–60Hz cadence.
      // Two independent settings want this, for different reasons:
      //   - lowPowerMode: minimise per-frame render work. The live
      //     volatile tail is still shown, just without the word-by-word
      //     animation.
      //   - streamingEnabled === false: the user opted out of live
      //     streaming and wants text to appear one committed markdown
      //     block at a time (ChatMarkdown withholds the volatile tail).
      //     For that gate to reflect WIRE arrival rather than a
      //     rate-limited crawl, the smoother must pass `received`
      //     straight through — otherwise a committed block would only
      //     surface after the animation had already inched through it.
      // The two stay orthogonal: low power governs the reveal ANIMATION;
      // the streaming toggle governs whether the in-progress block is
      // shown at all. All the onReveal invariants (live-content stamp,
      // reasoning tail, terminal auto-dispose, gate recompute) run
      // unchanged — the snap just delivers the whole backlog in one
      // reveal.
      revealImmediately: () =>
        getSettings().lowPowerMode || !getSettings().streamingEnabled,
      clock: getSmoothingClockForTest(),
      onReveal: (delta, _revealedEnd, previousCodeUnit) => runRevealTransaction(itemId, () => {
        const idx = options.getItemIndex(itemId);
        if (idx === undefined) {
          gate.disposeSmootherFor(itemId);
          return;
        }
        // A reveal is genuine live content advancing the bottom — stamp
        // so the controller spring-chases it. Runs every revealed frame,
        // INCLUDING the multi-second drain tail after the wire turn ends
        // (the smoother keeps revealing until caught up), which is what
        // makes the end-of-turn tail spring instead of jump.
        options.stampLiveContent();
        const current = options.getItems()[idx];
        const prevRevealed = previousRevealed;
        // Reasoning-tail rows (thinking + compaction_reasoning) keep the
        // summary tail-trimmed for memory; assistant_text keeps the full
        // revealed text.
        const isReasoningTail = isReasoningTailKind(current.kind);
        const settling = current.status !== 'streaming' && smoother.isCaughtUp();
        const routedThroughAssistantReveal = !isReasoningTail &&
          !settling &&
          current.summary === prevRevealed;
        if (routedThroughAssistantReveal) {
          assistantReveal.publish(
            itemId,
            previousCodeUnit,
            prevRevealed,
            delta,
            (nextSummary, mode, append) => {
              // Keep the smoother cursor and canonical row on the exact same
              // string. Building `prevRevealed + delta` independently here
              // created two growing cons trees per reveal and retained both
              // for the lifetime of the turn.
              const updatedAt = Math.max(latestUpdatedAt, current.updatedAt);
              switch (mode) {
                case 'direct':
                  options.appendDirectAssistantLiteral(
                    idx,
                    itemId,
                    append,
                    updatedAt,
                  );
                  break;
                case 'authoritative':
                  options.setItemAt(idx, {
                    ...current,
                    summary: nextSummary,
                    updatedAt,
                  });
                  break;
                default:
                  mode satisfies never;
              }
              previousRevealed = nextSummary;
            },
          );
        } else {
          // Keep the row's `updatedAt` monotonic. A status-only patch
          // (e.g. bare `{status: 'completed', updatedAt: T}`) can land
          // between deltas and bump `current.updatedAt` past the
          // smoother's last-known wire delta; the older value must not
          // overwrite it when the next rAF reveal lands.
          const updatedAt = Math.max(latestUpdatedAt, current.updatedAt);
          if (!isReasoningTail && current.summary === prevRevealed) {
            // Publish before the reactive write. Its reconciliation hook can
            // then preserve the complete pending direct suffix instead of
            // claiming only this last delta after an equal-length rewrite.
            assistantReveal.commitAuthoritativeAppend(
              itemId,
              prevRevealed,
              delta,
              (revealed) => {
                previousRevealed = revealed;
                options.setItemAt(idx, {
                  ...current,
                  summary: revealed,
                  updatedAt,
                });
              },
            );
          } else {
            const revealed = prevRevealed + delta;
            previousRevealed = revealed;
            if (!isReasoningTail) assistantReveal.discardItem(itemId);
            const nextSummary = isReasoningTail
              ? trimToTailRunes(revealed, THINKING_TAIL_RUNES)
              : revealed;
            const nextItem = {
              ...current,
              summary: nextSummary,
              updatedAt,
            };
            options.setItemAt(idx, nextItem);
            if (isReasoningTail) {
              registry.recordLiveTail(itemId, revealed);
              options.appendLivePayloadDeltaForItem(
                nextItem.id,
                reasoningExpansionStateKey(nextItem.kind),
                delta,
                thinkingPayloadVersionForItem(nextItem),
                prevRevealed,
              );
            }
          }
        }
        // Auto-cleanup once the stream has settled AND the smoother has
        // caught up. After that point no more deltas will arrive and
        // the smoother is dormant; holding the map slot would just
        // wait for the next thread switch. Terminal-status paths
        // (upsert reconcile and `applyItemPatch`'s snap branch) both
        // dispose synchronously before any further rAF fires, so this
        // never tramples an authoritative summary. The live tail is
        // retained — this reveal just wrote the summary as the trimmed
        // view of it, the definition of a content-consistent settle.
        if (smoother.isCaughtUp()) {
          // Advance the reveal gate even when terminal sink cleanup fails.
          // The smoother has already moved its cursor before this callback,
          // so returning with the old frontier would withhold every successor
          // despite there being no remaining reveal work.
          gate.mutateSmoothersAndRecompute(
            `streaming reveal settle for ${itemId}`,
            () => {
              if (current.status !== 'streaming') {
                registry.settleSmootherRetainingTail(itemId);
              }
            },
          );
        }
      }),
    });

    const entry: ItemSmoothing = {
      smoother,
      setLatestUpdatedAt(at) {
        latestUpdatedAt = at;
      },
    };
    itemSmoothers.set(itemId, entry);
    return entry;
  }

  /** applyItemDelta's smooth-kind path: get-or-create smoother seeded with
   *  currentSummary, push wire updatedAt, append the delta, recompute the gate. */
  function appendStreamingDelta(
    itemId: string,
    currentSummary: string,
    delta: string,
    updatedAt: number,
  ): void {
    const entry = getOrCreateSmoothing(itemId, currentSummary);
    entry.setLatestUpdatedAt(updatedAt);
    entry.smoother.appendDelta(delta);
    // A new smoothed row (or fresh lag on the frontier) may move the gate;
    // recompute so a withheld successor pauses behind the frontier.
    gate.recomputeReveal();
  }

  function applyPatchState(itemId: string, patch: ItemPatchEvent['patch']): void {
    const smoothing = itemSmoothers.get(itemId);
    const nextStatus = patch.status;
    // `errored`, `killed`, and `declined` all represent terminal
    // states where the user has either explicitly stopped the
    // stream or the provider failed it. In all three, we want the
    // already-streamed text to be fully visible before the patch's
    // summary (which may include an "[interrupted] " prefix or
    // similar) takes over — so snap synchronously and dispose.
    // Cancel / interrupt / error: synchronously reveal everything in
    // the smoother before applying the patch, then dispose. The
    // patch's own summary (e.g. "[interrupted] …") then lands as
    // the final visible text without being overwritten by a trailing
    // rAF tick.
    if (smoothing && isSnapStatus(nextStatus)) {
      registry.snapAndDisposeSmoother(itemId, smoothing);
    } else if (smoothing && patch.summary !== undefined) {
      // Status flipping to completed (or any non-snap patch) may
      // carry a final summary. If it extends what the smoother has
      // already received, push the suffix as a delta so the smoother
      // finishes the reveal naturally. If it doesn't extend (an
      // overwrite or a backwards correction), snap and dispose so
      // the patch's summary wins cleanly.
      const received = smoothing.smoother.getReceived();
      const patchSummary = patch.summary;
      // Reasoning-tail rows (thinking + compaction_reasoning) persist —
      // and settle with — the tail-trimmed preview, not the full text
      // (triage's thinkingSummaryPreview; both sides trim to the last
      // THINKING_TAIL_RUNES code points with no marker, so the strings
      // are byte-identical). A settle patch whose summary equals the
      // trimmed received text is a re-assert of what the smoother
      // already has, NOT an overwrite; treating it as a mismatch would
      // snap+dispose mid-drain and dump the unrevealed backlog
      // wholesale (the Codex thinking completion shape, and the
      // completion patch from persistCompletedBlockEmitStreaming).
      const item = options.getItemById(itemId);
      if (summaryRepresentsReceived(item?.kind, patchSummary, received)) {
        if (
          nextStatus !== undefined &&
          nextStatus !== 'streaming' &&
          smoothing.smoother.isCaughtUp()
        ) {
          // Terminal status AND nothing left to reveal. No further rAF
          // tick will fire, so the onReveal auto-cleanup can't dispose
          // — do it here or the smoother leaks until the next thread
          // switch. This is the completion shape wherever
          // content-block-stop carries ContentPresent=true (Codex
          // always; Claude recovered blocks): the settle re-asserts
          // the summary the smoother already received — content-
          // consistent by the equality check above, so the live tail
          // is retained. The bare-status branch below only covers the
          // case where that equal summary is OMITTED from the patch. A
          // not-yet-caught-up smoother keeps draining and disposes via
          // onReveal once it catches up (applyItemPatch skips the
          // direct summary write while it lives).
          registry.settleSmootherRetainingTail(itemId);
        }
      } else {
        const absorbed = absorbReceivedSuffix(
          smoothing,
          patchSummary,
          received,
          patch.updatedAt,
        );
        if (!absorbed) registry.snapAndDisposeSmoother(itemId, smoothing);
      }
    } else if (
      smoothing &&
      nextStatus !== undefined &&
      nextStatus !== 'streaming' &&
      smoothing.smoother.isCaughtUp()
    ) {
      // Bare status patch transitioning out of streaming with no
      // summary (e.g. `{status: 'completed', updatedAt: T}`). The
      // `onReveal` auto-cleanup only fires on a subsequent rAF tick;
      // if the smoother is already caught up, no further ticks will
      // arrive and the `itemSmoothers` entry would leak until the next
      // thread switch. The row's summary was last written by onReveal
      // as the trimmed view of the revealed text — content-consistent,
      // so the live tail is retained. Non-caught-up smoothers keep
      // streaming text and dispose via `onReveal` once they catch up.
      registry.settleSmootherRetainingTail(itemId);
    }

  }

  /** applyItemPatch's smoother decision tree followed by an UNCONDITIONAL
   *  recomputeReveal — even when no smoother exists for the id. */
  function applyPatch(itemId: string, patch: ItemPatchEvent['patch']): void {
    // Snap/dispose above may clear the frontier (interrupt, error,
    // completion). Derive the gate even when sink cleanup reports an error so
    // a failed reset cannot leave successor rows withheld indefinitely. This
    // still runs before applyItemPatch's early `itemsAreEqual` return.
    gate.mutateSmoothersAndRecompute(
      `streaming reveal patch for ${itemId}`,
      () => applyPatchState(itemId, patch),
    );
  }

  return {
    appendStreamingDelta,
    applyPatch,
  };
}
