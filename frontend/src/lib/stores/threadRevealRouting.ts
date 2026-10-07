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
import { adoptRevIfEqual } from './threadItems';
import { UNSTAMPED_ITEM_REV } from './threadWindowDigest';
import { classifyRevealText } from './threadRevealText';
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
import type { LiveRevealStream } from '../utils/payloadExpansion.svelte';
import { LiveTextWindow } from '../utils/liveText';
import { streamedTextPast, utf8Length } from '../utils/utf8Offsets';
import type { RevealGate } from './threadRevealGate.svelte';
import {
  isSnapStatus,
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
    streamEnd: number | undefined,
  ): void;
  /** Stamp the live-content latch (pane's stampLiveContent). */
  stampLiveContent(item: Item): void;
  /** rowUiState.appendLivePayloadDeltaForItem — live reasoning-tail payload append. */
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    stream: LiveRevealStream,
    delta: string,
    end: number,
    payloadVersion?: unknown,
  ): void;
  /**
   * A streaming row missed text: a delta or a patch named a position past
   * the text the pane holds. The owner re-reads its window; the read's row
   * and the held deltas close the gap.
   */
  onStreamGap?(itemId: string): void;
}

/**
 * A reasoning-tail smoother's revealed text, for the payload of an expanded
 * row. The payload keeps the stream past the smoother's disposal, so the
 * stream holds the smoother only until then.
 */
class SmootherRevealStream implements LiveRevealStream {
  private smoother: PerItemSmoother | null;

  constructor(smoother: PerItemSmoother) {
    this.smoother = smoother;
  }

  revealedText(end: number): string | null {
    return this.smoother?.getRevealed(end) ?? null;
  }

  release(): void {
    this.smoother = null;
  }
}

export interface RevealRouting {
  appendStreamingDelta(
    current: Item,
    delta: string,
    offset: number | undefined,
    updatedAt: number,
  ): void;
  applyPatch(itemId: string, patch: ItemPatchEvent['patch']): Item | null;
  /** A wholesale commit installed `rows`: place the deltas held for them. */
  afterRowsCommitted(rows: readonly Item[]): void;
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
    seedEnd: number | undefined,
  ): ItemSmoothing {
    const existing = itemSmoothers.get(itemId);
    if (existing) return existing;

    // A retained tail the seed resumes from ends where the row's summary
    // does, so `seedEnd` positions either seed.
    const seeded = registry.seedFromRetainedTail(itemId, initialReceived);
    let receivedEnd = seedEnd;
    // Where the revealed text ends, which every row write publishes as the
    // row's `streamEnd` while the row streams.
    let revealedStreamEnd = seedEnd;

    // Closure state for this item's smoother. Updated by each delta
    // and read inside `onReveal` so the row's `updatedAt` stays close
    // to wire time even as the smoother lags.
    let latestUpdatedAt = 0;
    // The stored row's revision once its text is all received; the settling
    // reveal publishes that text and so may carry it (`setSettledRev`).
    let settledRev: number | undefined;
    // Full previous revealed text of an assistant row. Appending each
    // emitted delta here keeps a canonical cons string without asking the
    // smoother to join its whole received buffer.
    let previousRevealed = seeded;
    // A reasoning-tail row's reveal state, created by its first reveal. The
    // row keeps a trimmed summary and the collapsed clamp a window of the
    // text, which is bounded while the text keeps containing newlines
    // (LiveTextWindow). The whole text stays in the smoother.
    let reasoning: {
      summary: string;
      window: LiveTextWindow;
      stream: SmootherRevealStream;
    } | null = null;

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
      onReveal: (delta, revealedEnd, previousCodeUnit) => runRevealTransaction(itemId, () => {
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
        const current = options.getItems()[idx];
        options.stampLiveContent(current);
        if (revealedStreamEnd !== undefined) revealedStreamEnd += utf8Length(delta);
        const streamEnd = current.status === 'streaming' ? revealedStreamEnd : undefined;
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
                    streamEnd,
                  );
                  break;
                case 'authoritative':
                  options.setItemAt(idx, {
                    ...current,
                    summary: nextSummary,
                    streamEnd,
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
          const rev = settling && settledRev !== undefined ? settledRev : current.rev;
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
                  streamEnd,
                  updatedAt,
                  rev,
                });
              },
            );
          } else if (isReasoningTail) {
            if (reasoning === null) {
              reasoning = {
                summary: trimToTailRunes(prevRevealed, THINKING_TAIL_RUNES),
                window: new LiveTextWindow(prevRevealed),
                stream: new SmootherRevealStream(smoother),
              };
              // The window and the smoother hold the text from here on.
              previousRevealed = '';
            }
            // Trimming the previous summary plus the delta equals trimming
            // the whole revealed text: the trim walks back from the end and
            // never starts inside a surrogate pair.
            reasoning.summary = trimToTailRunes(reasoning.summary + delta, THINKING_TAIL_RUNES);
            const nextItem = {
              ...current,
              summary: reasoning.summary,
              streamEnd,
              updatedAt,
              rev,
            };
            options.setItemAt(idx, nextItem);
            registry.recordLiveTail(itemId, reasoning.window.append(delta));
            options.appendLivePayloadDeltaForItem(
              nextItem.id,
              reasoningExpansionStateKey(nextItem.kind),
              reasoning.stream,
              delta,
              revealedEnd,
              thinkingPayloadVersionForItem(nextItem),
            );
          } else {
            const revealed = prevRevealed + delta;
            previousRevealed = revealed;
            assistantReveal.discardItem(itemId);
            options.setItemAt(idx, {
              ...current,
              summary: revealed,
              streamEnd,
              updatedAt,
              rev,
            });
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
      get receivedEnd() {
        return receivedEnd;
      },
      append(text) {
        smoother.appendDelta(text);
        if (receivedEnd !== undefined) receivedEnd += utf8Length(text);
        settledRev = undefined;
      },
      setLatestUpdatedAt(at) {
        latestUpdatedAt = at;
      },
      setSettledRev(rev) {
        settledRev = rev;
      },
      dispose() {
        try {
          smoother.dispose();
        } finally {
          reasoning?.stream.release();
        }
      },
    };
    itemSmoothers.set(itemId, entry);
    return entry;
  }

  /**
   * applyItemDelta's smooth-kind path: place the delta at its offset in the
   * row's stream, get-or-create the smoother seeded with the row, push wire
   * updatedAt, append, recompute the gate.
   *
   * Text the row already holds is dropped, and a delta that overlaps it
   * appends only its unseen part: a read of the row can hold deltas that
   * arrive after it. A delta that starts past the row's text is held, and
   * the owner re-reads the row (`onStreamGap`); joining it would drop the
   * text between. A delta or row without a position appends as it is.
   */
  function appendStreamingDelta(
    current: Item,
    delta: string,
    offset: number | undefined,
    updatedAt: number,
  ): void {
    const itemId = current.id;
    const existing = itemSmoothers.get(itemId);
    const end = existing ? existing.receivedEnd : current.streamEnd;
    let text = delta;
    if (offset !== undefined && end !== undefined) {
      const unseen = streamedTextPast(delta, offset, end);
      if (unseen === null) {
        registry.holdDelta(itemId, { offset, delta, updatedAt });
        options.onStreamGap?.(itemId);
        return;
      }
      if (unseen === '') return;
      text = unseen;
    }
    const entry = existing ?? getOrCreateSmoothing(itemId, current.summary, current.streamEnd);
    entry.setLatestUpdatedAt(updatedAt);
    entry.append(text);
    drainHeldDeltas(itemId);
    // A new smoothed row (or fresh lag on the frontier) may move the gate;
    // recompute so a withheld successor pauses behind the frontier.
    gate.recomputeReveal();
  }

  /**
   * Append the row's held deltas that now continue its text, in offset
   * order, up to the first that still starts past it. A row with no
   * position can place none of them.
   */
  function drainHeldDeltas(itemId: string): void {
    const held = registry.heldDeltas(itemId);
    if (!held) return;
    while (held.length > 0) {
      const current = options.getItemById(itemId);
      const entry = itemSmoothers.get(itemId);
      const end = entry ? entry.receivedEnd : current?.streamEnd;
      if (current === undefined || end === undefined) break;
      const next = held[0];
      const unseen = streamedTextPast(next.delta, next.offset, end);
      if (unseen === null) return;
      held.shift();
      if (unseen === '') continue;
      const target = entry ?? getOrCreateSmoothing(itemId, current.summary, current.streamEnd);
      target.setLatestUpdatedAt(next.updatedAt);
      target.append(unseen);
    }
    registry.dropHeldDeltas(itemId);
  }

  function afterRowsCommitted(rows: readonly Item[]): void {
    if (!registry.hasHeldDeltas()) return;
    for (const row of rows) {
      if (!registry.heldDeltas(row.id)) continue;
      if (options.getItemById(row.id)?.status === 'streaming') drainHeldDeltas(row.id);
      else registry.dropHeldDeltas(row.id);
    }
  }

  function applyPatchState(itemId: string, patch: ItemPatchEvent['patch']): void {
    const smoothing = itemSmoothers.get(itemId);
    if (!smoothing) return;
    if (isSnapStatus(patch.status)) {
      // Interrupt/error reveals pending text before the authoritative patch
      // takes ownership, including patches that omit their summary.
      registry.snapAndDisposeSmoother(itemId, smoothing);
      return;
    }
    if (patch.summary !== undefined) {
      const received = smoothing.smoother.getReceived();
      const relation = classifyRevealText(
        options.getItemById(itemId)?.kind, patch.summary, received,
      );
      if (relation === 'extension') {
        if (patch.updatedAt !== undefined) smoothing.setLatestUpdatedAt(patch.updatedAt);
        smoothing.append(patch.summary.slice(received.length));
        return;
      }
      if (relation !== 'same') {
        // Field patches are authoritative corrections, including shortened
        // text. Snapshot reconciliation deliberately has different authority.
        registry.snapAndDisposeSmoother(itemId, smoothing);
        return;
      }
    }
    // A bare status and a content-consistent summary (including a trimmed
    // reasoning preview) settle identically. If already caught up, no further
    // frame will dispose the smoother; otherwise onReveal owns final cleanup.
    if (patch.status !== undefined && patch.status !== 'streaming' &&
      smoothing.smoother.isCaughtUp()) {
      registry.settleSmootherRetainingTail(itemId);
    }
  }

  /** Own the entire patch: reconcile text, commit lifecycle, then derive the gate. */
  function applyPatch(itemId: string, patch: ItemPatchEvent['patch']): Item | null {
    let committed: Item | null = null;
    gate.mutateSmoothersAndRecompute(`streaming reveal patch for ${itemId}`, () => {
      const index = options.getItemIndex(itemId);
      if (index === undefined) {
        registry.disposeRemovedItem(itemId);
        return;
      }
      const current = options.getItems()[index];
      const smoothing = itemSmoothers.get(itemId);
      const heldEnd = smoothing ? smoothing.receivedEnd : current.streamEnd;
      applyPatchState(itemId, patch);
      // Snap may have published text synchronously. Always read its result;
      // spreading the pre-snap row would silently restore the shorter text.
      const next = { ...options.getItems()[index] };
      if (patch.status !== undefined) next.status = patch.status;
      if (patch.summary !== undefined) next.summary = patch.summary;
      if (patch.meta !== undefined) next.meta = patch.meta;
      if (patch.decision !== undefined) next.decision = patch.decision;
      if (patch.updatedAt !== undefined) next.updatedAt = patch.updatedAt;
      // The patching write moved the row's revision; carrying it is what
      // makes a settled streaming row describable again — its upsert
      // arrived at `rev: -1` because the wire row was altered
      // (docs/architecture/thread-replica-sync.md §3.1). The revision
      // describes the stored row, so a row whose text ends elsewhere keeps
      // its own: a later open would verify the held text as the stored one.
      const holdsStoredText = patch.summary !== undefined || patch.streamEnd === undefined
        || heldEnd === patch.streamEnd;
      const revealing = itemSmoothers.get(itemId);
      if (revealing && !revealing.smoother.isCaughtUp()) {
        // The row shows the reveal cursor, short of the text the revision
        // names. Carrying it would let a cached window that a thread switch
        // left mid-reveal verify as fresh.
        revealing.setSettledRev(holdsStoredText ? patch.rev : undefined);
        next.rev = UNSTAMPED_ITEM_REV;
      } else {
        next.rev = holdsStoredText ? patch.rev : current.rev;
      }
      if (patch.summary === undefined && patch.streamEnd !== undefined
        && heldEnd !== undefined && heldEnd < patch.streamEnd) {
        registry.markStale(itemId);
        options.onStreamGap?.(itemId);
      }
      if (itemSmoothers.has(itemId)) {
        next.summary = options.getItems()[index].summary;
      } else if (patch.summary !== undefined) {
        // The patch's text owns the row, at a position it does not name.
        next.streamEnd = undefined;
        registry.clearStale(itemId);
      }
      if (next.status !== 'streaming') {
        next.streamEnd = undefined;
        registry.dropHeldDeltas(itemId);
      }
      // A patch that moves only the revision (a re-persist of an
      // unchanged row) is absorbed onto the held row: no replacement,
      // no reactive write, rev carried.
      if (adoptRevIfEqual(current, next)) return;
      if (next.summary !== current.summary) options.stampLiveContent(next);
      options.setItemAt(index, next);
      committed = next;
    });
    return committed;
  }

  return {
    appendStreamingDelta,
    applyPatch,
    afterRowsCommitted,
  };
}
