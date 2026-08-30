// stores/threadPaneRevealSmoothing.test.ts
//
// The reveal PACING half of threadStreamingReveal: per-tick advance under
// the adaptive catch-up curve, the 400-rune thinking tail cap, and the
// visibility-resume snap. Ordering across rows is threadRevealSequencer;
// the invariant itself is threadStreamingRevealInvariant.test.ts.

import { beforeEach, describe, expect, it } from 'vitest';
import { __setSmoothingClockForTest } from './thread.svelte';
import {
  MAX_ADAPTIVE_CHARS_PER_SEC,
  MAX_ADVANCE_PER_TICK_CHARS,
} from '../markdown/smoothing/PerItemSmoother';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import {
  FakeSmoothingClock,
  installThreadPaneTestEnv,
  smoothingNewTailChars,
} from '../../test/helpers/threadPane';

describe('reveal smoothing', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('thinking smoothing past the 400-rune tail cap', () => {
    function buildWords(n: number): string[] {
      // Short ~5-char words separated by spaces. ~6 chars per word means
      // 70 words ≈ 420 chars — enough to push past THINKING_TAIL_RUNES=400.
      const out: string[] = [];
      for (let i = 0; i < n; i++) out.push(`word${String(i).padStart(2, '0')}`);
      return out;
    }

    it('keeps writing items[].summary in word-sized advances after revealed > 400 runes', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think' }));
        // Simulate the firstBlock upsert (Go-side initial thinking row),
        // then a long sequence of wire deltas — same shape Claude produces
        // for reasoning that flows past the 400-rune tail.
        const initial = 'seed ';
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: initial,
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        const words = buildWords(80); // ~7 chars × 80 ≈ 560 chars total.
        // Stream each word as its own wire delta with a few rAF frames
        // between them. This mimics Claude's bursty reasoning text where
        // 5–50 char chunks arrive every 30–100 ms.
        const summaryAtTick: { tick: number; len: number }[] = [];
        let frameCount = 0;
        for (let i = 0; i < words.length; i++) {
          pane.applyItemDelta({
            threadId: 'thread-think',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          // Run a handful of rAF frames per wire delta so the smoother
          // gets a chance to reveal between deltas. 6 frames × 16ms = 96ms.
          for (let f = 0; f < 6; f++) {
            clock.tickFrame(16);
            frameCount++;
            summaryAtTick.push({
              tick: frameCount,
              len: pane.items[0].summary.length,
            });
          }
        }
        // Drain remaining lag.
        while (clock.pendingCount() > 0) {
          clock.tickFrame(16);
          frameCount++;
          summaryAtTick.push({
            tick: frameCount,
            len: pane.items[0].summary.length,
          });
        }

        // Sanity: by the end, summary should equal the trimmed tail of the
        // full received text.
        const fullText = initial + words.map((w) => w + ' ').join('');
        const expectedTail = fullText.slice(-400);
        expect(pane.items[0].summary).toBe(expectedTail);

        // The smoother is the *only* writer to items[idx].summary for
        // thinking. Per-tick advances after each reveal land in word-sized
        // increments at the base rate (160 cps × 16ms ≈ 2.5 chars; word
        // units round up to ~7 chars). If anything in the pipeline starts
        // bypassing the smoother past the trim cap, we'd see a jump
        // equal to one wire delta (~7 chars) appear "all at once" without
        // the matching per-tick growth that precedes it.
        //
        // Find every transition where summary GREW (length increased).
        // Before the trim engages (summary < 400), growth jumps are
        // exactly the word advance. After the trim engages, summary
        // stays pinned at 400 chars but its CONTENT shifts — the
        // length-delta-only check no longer suffices, so we instead
        // verify that *no single tick* added more than ~14 chars (2
        // word-units worth) to either the length OR the trailing slice.
        let maxLengthJump = 0;
        let maxContentJump = 0;
        let prevSummary = initial;
        // Walk all rAF ticks again to also inspect content (not just len).
        // We approximate by replaying from the recorded snapshot: read the
        // *current* summary after each tick. But pane state has progressed
        // past the loop, so use the final state for content-jump checks
        // via getOrCreateSmoothing's revealed history — we don't have
        // that here. Instead, do a SECOND clean run with a fresh pane and
        // capture summary at each frame.
        {
          // Reset and re-run with snapshot capture.
          const clock2 = new FakeSmoothingClock();
          __setSmoothingClockForTest(clock2);
          const pane2 = await buildPane(makeThread({ id: 'thread-think-2' }));
          pane2.upsertItem(
            makeItem({
              id: 'think:0:0',
              threadId: 'thread-think-2',
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: initial,
              payloadId: 'thinking:think:0:0',
              updatedAt: 1,
            }),
          );
          let prev = initial;
          for (let i = 0; i < words.length; i++) {
            pane2.applyItemDelta({
              threadId: 'thread-think-2',
              itemId: 'think:0:0',
              kind: 'thinking',
              delta: words[i] + ' ',
              updatedAt: 100 + i,
            });
            for (let f = 0; f < 6; f++) {
              clock2.tickFrame(16);
              const cur = pane2.items[0].summary;
              // Length jump (positive only — trim might shrink it back
              // to 400, which we don't penalize).
              const lenJump = Math.max(0, cur.length - prev.length);
              maxLengthJump = Math.max(maxLengthJump, lenJump);
              // Content jump: how much new text appeared at the END
              // relative to the previous summary. After trim, prev and
              // cur are both 400-char tails; new content is the part of
              // cur that doesn't overlap prev as a suffix-of-prev
              // prefix-of-cur match.
              const contentJump = smoothingNewTailChars(prev, cur);
              maxContentJump = Math.max(maxContentJump, contentJump);
              prev = cur;
            }
          }
          while (clock2.pendingCount() > 0) {
            clock2.tickFrame(16);
            const cur = pane2.items[0].summary;
            const lenJump = Math.max(0, cur.length - prev.length);
            maxLengthJump = Math.max(maxLengthJump, lenJump);
            const contentJump = smoothingNewTailChars(prev, cur);
            maxContentJump = Math.max(maxContentJump, contentJump);
            prev = cur;
          }
          // Reference the unused vars so lint stays clean.
          void prevSummary;
          void summaryAtTick;
        }
        // Word units in our test are 7 chars (e.g. "word00 "). Adaptive
        // catch-up can fire several word units in one tick when lag is
        // high, but should not approach the ~50+ chars/tick that wire
        // deltas would produce if the smoother were bypassed. Cap at
        // 28 chars (~4 word units in one frame) — well below "5 words
        // appearing as a chunk".
        expect(maxLengthJump).toBeLessThanOrEqual(28);
        expect(maxContentJump).toBeLessThanOrEqual(28);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not produce wire-chunk-sized reveals when the wire bursts faster than the base rate', async () => {
      // Reproduces the user-reported regression: past ~400 chars,
      // streamed text appears in chunks "exactly like the old behavior
      // before any smoothing changes" — 5 words, pause, 15 words. The
      // hypothesis is that the adaptive catch-up math (`drain lag in
      // 500ms`) scales the per-tick reveal proportional to lag, so a wire
      // that bursts faster than the 160 cps base rate eventually settles
      // at a steady-state lag where per-tick = wire_rate * (16/500) — for
      // a 2000 cps wire, that's 64 chars (~10 words) per tick.
      //
      // Run on assistant_text; the reasoning counterpart is the next test.
      // The guarantee is kind-independent — no smoother skips or chunks —
      // so both kinds hold the same per-tick cap.
      //
      // Wire pattern is realistic: 50-char wire bursts arriving every
      // 25ms (= 2000 cps sustained, close to Claude's burst rate for
      // reasoning text). Streamed for ~1.5s so we reach steady-state lag.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-burst' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-burst',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );

        // 50-char wire bursts with realistic word distribution (5–8 char
        // words separated by spaces). Each burst is ~7 words.
        function makeBurst(seed: number): string {
          const sizes = [4, 7, 5, 6, 8, 5, 7];
          const out: string[] = [];
          let used = 0;
          let i = 0;
          while (used < 50) {
            const sz = sizes[(seed + i) % sizes.length];
            const word = 'a'.repeat(sz);
            out.push(word);
            used += sz + 1; // +1 for space
            i++;
          }
          return out.join(' ') + ' ';
        }

        let maxContentJump = 0;
        let maxLengthJump = 0;
        let prev = '';
        let burstIdx = 0;
        // Wire arrives every 25ms; tick rAF every 16ms. We loop over a
        // 1500ms simulated window, emitting a wire burst on the 25ms
        // cadence and a rAF on the 16ms cadence (interleaved by time).
        const totalMs = 1500;
        const wireIntervalMs = 25;
        const rafIntervalMs = 16;
        let nextWireAt = 0;
        let nextRafAt = 0;
        let elapsed = 0;
        const measure = () => {
          const cur = pane.items[0].summary;
          const lenJump = Math.max(0, cur.length - prev.length);
          const contentJump = smoothingNewTailChars(prev, cur);
          maxLengthJump = Math.max(maxLengthJump, lenJump);
          maxContentJump = Math.max(maxContentJump, contentJump);
          prev = cur;
        };
        while (elapsed < totalMs) {
          if (nextWireAt <= nextRafAt) {
            const dt = nextWireAt - elapsed;
            if (dt > 0) {
              clock.tickFrame(dt);
              elapsed += dt;
              // Measure after every clock advance so per-tick reveals
              // are observed individually — without this, several
              // smoother ticks could fire between two rAF-branch
              // measurements and the recorded "jump" would be the sum
              // of all of them.
              measure();
            }
            const burst = makeBurst(burstIdx++);
            pane.applyItemDelta({
              threadId: 'thread-burst',
              itemId: 'text:0:0',
              kind: 'assistant_text',
              delta: burst,
              updatedAt: 100 + burstIdx,
            });
            nextWireAt = elapsed + wireIntervalMs;
          } else {
            const dt = nextRafAt - elapsed;
            if (dt > 0) {
              clock.tickFrame(dt);
              elapsed += dt;
              measure();
            }
            nextRafAt = elapsed + rafIntervalMs;
          }
        }
        // Drain any remaining lag.
        while (clock.pendingCount() > 0) {
          clock.tickFrame(16);
          measure();
        }

        // "5 words show up" ≈ 30 chars; "15 more words" ≈ 90 chars.
        // A healthy smoother stays under the per-tick work cap (~3
        // short words) even under steady-state burst. The cap inside
        // `PerItemSmoother.tick()` is what enforces this; without it,
        // adaptive math at lag ~= wire_rate * (catchup_ms / 1000)
        // produces 60–100+ chars/tick under sustained 2000 cps bursts
        // and the user perceives those as chunks of 5–15 words.
        expect(maxLengthJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
        expect(maxContentJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never chunks a reasoning row either — and the wire gap drains it to zero', async () => {
      // A collapsed reasoning row gets the SAME guarantee as prose: every
      // character animates, in order, at no more than the per-tick cap.
      // A bounded-backlog skip for reasoning rows was implemented and
      // rejected — dropping characters the reader might expand into is
      // worse than making a queued row wait.
      //
      // The second half is why the wait is acceptable: the wire is bursty.
      // Once the overspeed burst stops (tool call, API round-trip, model
      // pause), the drain keeps running and returns the row to zero lag.
      // If this test is ever "fixed" by capping the backlog, read the
      // rationale in PerItemSmoother.ts first.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-bound' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-bound',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        // ~50 chars every 16ms ≈ 3000 cps, an order above the reveal
        // ceiling, sustained for 100 frames.
        const burst = 'word '.repeat(10);
        let received = '';
        let maxJump = 0;
        let previousRevealed = 0;
        for (let i = 0; i < 100; i++) {
          received += burst;
          pane.applyItemDelta({
            threadId: 'thread-think-bound',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: burst,
            updatedAt: 100 + i,
          });
          clock.tickFrame(16);
          const revealed = (pane.liveThinkingTailForItem('think:0:0') ?? '')
            .length;
          maxJump = Math.max(maxJump, revealed - previousRevealed);
          previousRevealed = revealed;
        }
        // No frame delivered more than a tick's animated work — the wire
        // ran far ahead, and the row simply fell behind rather than
        // jumping to catch it.
        expect(maxJump).toBeLessThanOrEqual(MAX_ADVANCE_PER_TICK_CHARS);
        // It genuinely fell behind, so the drain below is not vacuous.
        const lagAtBurstEnd = received.length - previousRevealed;
        expect(lagAtBurstEnd).toBeGreaterThan(MAX_ADVANCE_PER_TICK_CHARS * 10);

        // The gap: the wire stops, frames keep coming. The backlog drains
        // to zero at the ceiling — no skip needed, and the reader gets
        // every character.
        let gapFrames = 0;
        while (clock.pendingCount() > 0 && gapFrames < 20000) {
          clock.tickFrame(16);
          gapFrames++;
        }
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(received);
        expect(pane.items[0].summary).toBe(received.slice(-400));
        // Drained at the reveal ceiling, not faster: a rush regime would
        // finish materially sooner than the rate implies.
        expect(gapFrames * 16).toBeGreaterThan(
          (lagAtBurstEnd / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000 * 0.9,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reveals a single small wire delta over multiple ticks past 400 runes', async () => {
      // Reproduces the user's "5 words in one chunk even when only 5
      // words streamed" report. The smoother is past the trim threshold
      // and caught up (revealed == received, lag = 0). A SINGLE small
      // wire delta (≈5 words) arrives. With base rate 160 cps and the
      // per-tick cap of 14 chars, those 5 words must reveal over at
      // least ~5 rAF ticks (~80ms), never as one DOM update.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-burst' }));
        // Seed the item with > 400 chars already in the summary so the
        // trim is already engaged. The smoother starts caught up
        // (initialRevealed = initialReceived = seed), so this isolates
        // the per-tick reveal of the NEXT delta.
        const seedWords: string[] = [];
        for (let i = 0; i < 80; i++)
          seedWords.push(`word${String(i).padStart(2, '0')}`);
        const seed = seedWords.join(' ') + ' ';
        expect(seed.length).toBeGreaterThan(400);
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-burst',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: seed,
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // Seed the smoother by sending a zero-impact delta. The
        // production path creates the smoother in applyItemDelta with
        // initialReceived = current.summary = seed; revealed = seed.
        // Lag = delta.length. We feed a single small 5-word burst.
        const fiveWords = 'hello bright cosmic future today ';
        pane.applyItemDelta({
          threadId: 'thread-think-burst',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fiveWords,
          updatedAt: 2,
        });

        // Walk rAF ticks and record per-tick summary changes. Cap the
        // walk well past the expected drain so we can verify the
        // smoother caught up at the end.
        const tickAdvances: number[] = [];
        let prev = pane.items[0].summary;
        for (let i = 0; i < 30 && clock.pendingCount() > 0; i++) {
          clock.tickFrame(16);
          const cur = pane.items[0].summary;
          const advance = smoothingNewTailChars(prev, cur);
          if (advance > 0) tickAdvances.push(advance);
          prev = cur;
        }

        // Verify: the 5 words (33 chars) revealed over MULTIPLE ticks,
        // with each tick's advance bounded by the cap. None should be
        // the full 33-char delta.
        expect(tickAdvances.length).toBeGreaterThanOrEqual(2);
        for (const advance of tickAdvances) {
          expect(advance).toBeLessThanOrEqual(14);
        }
        // Sanity: the trailing 5 words are now in the summary.
        expect(pane.items[0].summary.endsWith(fiveWords)).toBe(true);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drains the remaining smoother backlog after status flips to completed with an extending summary', async () => {
      // The per-tick cap means catch-up can no longer outrun the wire
      // — accumulated lag at completion time must still drain to the
      // patch's extending summary. Verify the applyItemPatch
      // extending-summary branch appends the suffix as a delta, the
      // smoother continues at the capped rate, and the on-reveal
      // auto-cleanup (`!streaming && isCaughtUp`) eventually fires so
      // the row settles with the full final text and the smoother map
      // doesn't strand a stale entry.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-drain' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-drain',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // Stream the first half (~150 chars) as deltas, then complete
        // with an extending summary that adds another ~150 chars on
        // top. This is the actual extending-summary path: smoother
        // received < patchSummary AND patchSummary.startsWith(received).
        const allWords: string[] = [];
        for (let i = 0; i < 50; i++)
          allWords.push(`item${String(i).padStart(2, '0')}`);
        const fullText = allWords.join(' ') + ' ';
        const streamed = allWords.slice(0, 25).join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-drain',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: streamed,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-drain',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: fullText, updatedAt: 3 },
        });

        // Drain. With per-tick cap = 14 chars, ~300 chars takes ~22
        // ticks (~350ms). Allow more to be safe.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) {
          clock.tickFrame(16);
        }
        // Final state: full text revealed (trimmed), status flipped,
        // smoother auto-disposed (no leftover pending callbacks).
        expect(pane.items[0].summary).toBe(fullText.slice(-400));
        expect(pane.items[0].status).toBe('completed');
        expect(clock.pendingCount()).toBe(0);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // The onReveal auto-cleanup settles RETAINING the tail: the
        // extending summary drained fully, so the full final text keeps
        // serving past the settle (content-consistent with the trimmed
        // summary recorded above).
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps draining when the completion patch carries the trimmed tail preview', async () => {
      // Thinking rows persist the tail-trimmed preview as their summary
      // (Go thinkingSummaryPreview mirrors THINKING_TAIL_RUNES), so a
      // content-present settle patch re-asserts the TRIMMED text — not
      // the full received stream. Mid-drain, past 400 runes, that patch
      // summary neither equals nor extends the smoother's received text;
      // treating it as an overwrite snap+disposes the smoother and dumps
      // the unrevealed backlog wholesale (the Codex thinking completion
      // shape, and the recovered-block settle patch on Claude). The
      // patch must instead read as a re-assert: smoother survives, keeps
      // draining at the capped rate, and auto-disposes at catch-up.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-think-settle' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-think-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        // > 400 runes so the trimmed preview provably differs from the
        // received text, delivered as one delta so the smoother holds a
        // large backlog when the settle patch lands.
        const full = buildWords(80).join(' ') + ' '; // 560 chars
        expect(full.length).toBeGreaterThan(400);
        pane.applyItemDelta({
          threadId: 'thread-think-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: full,
          updatedAt: 2,
        });
        // A couple of frames in: genuinely mid-drain.
        clock.tickFrame(16);
        clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        const midDrain = pane.items[0].summary;
        expect(midDrain.length).toBeLessThan(full.length);

        // The settle patch as Go emits it: completed + trimmed preview.
        pane.applyItemPatch({
          threadId: 'thread-think-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: full.slice(-400), updatedAt: 3 },
        });

        // Smoother survives; the patch neither snapped the reveal nor
        // wrote the trimmed preview over the mid-drain summary.
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(pane.items[0].summary).toBe(midDrain);
        expect(pane.items[0].status).toBe('completed');

        // Drain to completion: converges on the trimmed tail and the
        // onReveal auto-cleanup disposes the smoother.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) {
          clock.tickFrame(16);
        }
        expect(pane.items[0].summary).toBe(full.slice(-400));
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('exposes a monotonically-growing live tail past 400 runes for the collapsed view', async () => {
      // Regression guard for the user-reported "5 words appear at once
      // past 400 runes" symptom. The collapsed ThinkingBlock renders a
      // `<span>{bodyText}</span>` inside `whitespace-pre-wrap` +
      // `max-h-[3lh] overflow-hidden` + `scrollTop = scrollHeight`.
      // When bodyText is `item.summary` past the trim threshold, the
      // string is a sliding window — characters drop from the start as
      // new ones arrive at the end. Even a single 1-char-per-tick
      // reveal recomputes wrap for the full bounded string and can
      // shift the visible 3 lines wholesale when a word at the start
      // crosses a wrap boundary. `pane.liveThinkingTailForItem` exposes
      // the smoother's full revealed text instead, which grows append-
      // only — wrap layout never reshuffles older text and the visible
      // window scrolls by exactly the per-tick reveal.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-live-tail' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-live-tail',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );

        // Stream enough text to push well past 400 runes.
        const words: string[] = [];
        for (let i = 0; i < 100; i++)
          words.push(`tok${String(i).padStart(2, '0')}`);
        // Feed in word-by-word so the smoother has lag throughout.
        for (let i = 0; i < words.length; i++) {
          pane.applyItemDelta({
            threadId: 'thread-live-tail',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          clock.tickFrame(16);
        }
        // Drain remaining lag.
        for (let frame = 0; clock.pendingCount() > 0 && frame < 100; frame++) {
          clock.tickFrame(16);
        }
        expect(clock.pendingCount()).toBe(0);

        const finalTail = pane.liveThinkingTailForItem('think:0:0');
        // Smoother is still live (status === 'streaming') so the live
        // tail must be populated and equal the full received text.
        expect(finalTail).not.toBeNull();
        expect(finalTail!.length).toBeGreaterThan(400);
        // items[].summary is the trimmed sliding window; live tail is the
        // full text. They must diverge in length once past the cap —
        // proving the collapsed render no longer reads the bounded
        // sliding-window source.
        expect(pane.items[0].summary.length).toBeLessThanOrEqual(400);
        expect(finalTail!.length).toBeGreaterThan(pane.items[0].summary.length);

        // Now sample monotonic growth across a fresh run: at each tick
        // the live tail must be a prefix-extension of the previous tail
        // (append-only, never sliding window).
        const clock2 = new FakeSmoothingClock();
        __setSmoothingClockForTest(clock2);
        const pane2 = await buildPane(makeThread({ id: 'thread-live-tail-2' }));
        pane2.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-live-tail-2',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        let prev = '';
        let pastTrimSamples = 0;
        let growthPastTrimSamples = 0;
        for (let i = 0; i < words.length; i++) {
          pane2.applyItemDelta({
            threadId: 'thread-live-tail-2',
            itemId: 'think:0:0',
            kind: 'thinking',
            delta: words[i] + ' ',
            updatedAt: 100 + i,
          });
          for (let f = 0; f < 3; f++) {
            clock2.tickFrame(16);
            const cur = pane2.liveThinkingTailForItem('think:0:0') ?? '';
            if (cur.length > 0) {
              // Append-only invariant: previous tail is always a prefix
              // of the new tail (no characters drop from the start).
              expect(cur.startsWith(prev)).toBe(true);
              if (cur.length > 400) pastTrimSamples++;
              // Real growth past the trim threshold (not the smoother
              // sitting idle re-reading the same value) — guards
              // against a regression that quietly clamps the live tail
              // to the trimmed-summary length.
              if (cur.length > 400 && cur.length > prev.length)
                growthPastTrimSamples++;
              prev = cur;
            }
          }
        }
        // We must have actually crossed the 400-rune threshold while
        // sampling — otherwise the test doesn't exercise the regression
        // path it claims to.
        expect(pastTrimSamples).toBeGreaterThan(10);
        // And the tail must have grown past the threshold more than
        // once: a single growth tick crossing 400 followed by an idle
        // smoother would still satisfy `pastTrimSamples > 10` because
        // the same value is re-read many times. Real append-only
        // behaviour produces growth on most reveals.
        expect(growthPastTrimSamples).toBeGreaterThan(5);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('retains the live thinking tail when the smoother disposes on completion', async () => {
      // Once the stream settles the smoother auto-disposes, but the live
      // tail entry is RETAINED: the collapsed clamp is rendering exactly
      // that string, and swapping to the trimmed summary at settle
      // re-wraps the visible 3 lines in front of the reader (wrap
      // depends on where the string starts; the trim starts
      // mid-sentence). The offscreen row-UI prune bounds the retention
      // (see the prune test below) — settle-time is the one moment the
      // swap must NOT happen.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-cleanup' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-tail-cleanup',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++)
          words.push(`tok${String(i).padStart(2, '0')}`);
        const fullText = words.join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-tail-cleanup',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fullText,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-tail-cleanup',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: fullText, updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.items[0].status).toBe('completed');
        // Smoother disposed (the resource cleanup the settle owes) …
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // … but the tail is retained byte-identical to the last reveal,
        // and diverges from the trimmed summary (fullText > 400 runes),
        // proving the collapsed render did not swap sources at settle.
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);
        expect(pane.items[0].summary.length).toBeLessThan(fullText.length);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('disposes the smoother on a bare status-completed patch with no summary', async () => {
      // Regression for a leak in applyItemPatch: a status-only patch
      // (e.g. Codex sometimes sends `{status: 'completed', updatedAt}`
      // without re-asserting `summary` when the wire summary already
      // matched what the smoother had received) took neither the snap
      // branch (status isn't errored/killed/declined) nor the
      // extend-or-snap branch (no summary). The `onReveal` auto-cleanup
      // at the smoother factory site only runs on a subsequent rAF
      // tick, so a smoother that's already caught up by the time the
      // patch lands would never re-fire — the `itemSmoothers` entry
      // (and its zombie rAF scheduling) leaked until the next thread
      // switch. The live TAIL, by contrast, is deliberately retained on
      // this content-consistent settle: the leak's harm was the
      // undisposed smoother, and the retained string is what keeps the
      // collapsed clamp from re-wrapping at the settle boundary.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-bare-status' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-bare-status',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-bare-status',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'reasoning text ',
          updatedAt: 2,
        });
        // Drain to caught-up so the next onReveal auto-cleanup branch
        // is unreachable — only the patch handler can dispose now.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.applyItemPatch({
          threadId: 'thread-bare-status',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('reasoning text ');
        // Drain again to confirm no zombie rAF ticks (a fresh tick
        // after a leak would re-fire onReveal against the disposed
        // slot); the retained tail must stay byte-stable through it.
        safety = 20;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('reasoning text ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the retained tail when a snap-status patch overwrites the summary', async () => {
      // Tail retention is for content-consistent settles ONLY. A
      // kill/error patch rewrites the summary (e.g. an "[interrupted] "
      // prefix); a retained tail would keep the collapsed clamp showing
      // the pre-patch text and mask the authoritative summary.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-snap-drop' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-snap-drop',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-snap-drop',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'partial reasoning ',
          updatedAt: 2,
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.applyItemPatch({
          threadId: 'thread-snap-drop',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: {
            status: 'killed',
            summary: '[interrupted] partial reasoning ',
            updatedAt: 3,
          },
        });

        expect(pane.items[0].summary).toBe('[interrupted] partial reasoning ');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops a retained tail when the settled row is removed', async () => {
      // Guards the dispose-order fix: a settled row has a retained tail
      // but NO smoother, and disposeSmootherFor used to early-return on
      // the missing smoother before touching the tail map — a removal
      // after settle would have leaked the string until thread switch.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-remove-tail' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-remove-tail',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-remove-tail',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'settled reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-remove-tail',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.removeItemById('think:0:0', 'thread-remove-tail');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('prunes retained tails offscreen, never live ones', async () => {
      // The offscreen row-UI prune is what bounds tail retention. A
      // settled tail outside the retention set drops; a STREAMING row's
      // tail survives regardless of retention — the live reveal owns it.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-prune' }));
        for (const [itemIndex, id] of (['think:0:0', 'think:0:1'] as const).entries()) {
          pane.upsertItem(
            makeItem({
              id,
              threadId: 'thread-tail-prune',
              turnIndex: 0,
              itemIndex,
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: '',
              payloadId: `thinking:${id}`,
              updatedAt: 1,
            }),
          );
        }
        // Settle the first row (tail retained, smoother gone).
        pane.applyItemDelta({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'first reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        // Stream the second row and drain: the smoother catches up but
        // stays LIVE (no terminal status arrived), so its tail is owned
        // by the reveal, not the settled map.
        pane.applyItemDelta({
          threadId: 'thread-tail-prune',
          itemId: 'think:0:1',
          kind: 'thinking',
          delta: 'second reasoning ',
          updatedAt: 4,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first reasoning ');
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');

        // Retention keeps the settled row → both tails survive.
        pane.pruneRowUiState({ itemIds: new Set(['think:0:0']), payloads: new Set<string>(), groupKeys: new Set() });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first reasoning ');
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');

        // Empty retention: the settled tail drops; the live one is owned
        // by its smoother and must survive.
        pane.pruneRowUiState({ itemIds: new Set(), payloads: new Set<string>(), groupKeys: new Set() });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe('second reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('invalidates a retained tail when a terminal re-upsert rewrites the summary', async () => {
      // THE consistency case retention must survive: triage re-persists
      // a completed thinking row when a late content-present stop's
      // text differs (persistOrUpdateCompletedThinkingItem), and that
      // upsert lands on a row that already settled — no smoother, no
      // reconcile entry, nothing writer-side to notice. The read-time
      // validation in liveThinkingTailFor is what catches it: the
      // summary recorded at settle no longer matches the row, so the
      // stale tail must stop being served.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-divergent-upsert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-divergent-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'original reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-divergent-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('original reasoning ');

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: 'authoritative rewritten reasoning',
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        expect(pane.items[0].summary).toBe('authoritative rewritten reasoning');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('invalidates a retained tail when a correction patch rewrites the settled summary', async () => {
      // Same consistency story as the re-upsert test, through the patch
      // path: a post-settle correction rewrites items[].summary with no
      // smoother alive to observe it.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-divergent-patch' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-divergent-patch',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'settled reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('settled reasoning ');

        pane.applyItemPatch({
          threadId: 'thread-divergent-patch',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { summary: 'corrected reasoning', updatedAt: 4 },
        });
        expect(pane.items[0].summary).toBe('corrected reasoning');
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps serving a retained tail through a consistent summary re-assert', async () => {
      // The validation must be a consistency check, not a
      // one-shot fuse: a patch that re-asserts the SAME summary the
      // settle recorded (Claude terminal replays do this) leaves the
      // rendered string untouched, so the tail keeps serving.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-reassert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reassert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'stable reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('stable reasoning ');

        pane.applyItemPatch({
          threadId: 'thread-reassert',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', summary: pane.items[0].summary, updatedAt: 4 },
        });
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('stable reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reseeds a resumed smoother from the retained tail when the summary is unchanged', async () => {
      // A replay upsert can flip a settled row back to streaming and
      // follow with deltas (turn resume). The fresh smoother must seed
      // from the retained FULL tail, not the trimmed summary — seeding
      // from the summary would shrink the rendered string and re-wrap
      // the clamp, the exact jump retention exists to prevent.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-reseed' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++) words.push(`tok${String(i).padStart(2, '0')}`);
        const fullText = words.join(' ') + ' ';
        pane.applyItemDelta({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: fullText,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        const trimmedSummary = pane.items[0].summary;
        // >400 runes, so the trim is a real shrink — the seed choice is
        // observable.
        expect(trimmedSummary.length).toBeLessThan(fullText.length);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(fullText);

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: trimmedSummary,
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'resumed ',
          updatedAt: 5,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(`${fullText}resumed `);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops a stale retained tail when the row resumes with a rewritten summary', async () => {
      // The reseed's negative: if the resuming row's summary is NOT the
      // one the settle recorded, the retained tail belongs to a dead
      // version of the row. The seed must start from the new summary
      // and clear the stale entry rather than shadow the resumed reveal.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-stale-reseed' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-stale-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'first life reasoning ',
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'completed', updatedAt: 3 },
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('first life reasoning ');

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-stale-reseed',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: 'rewritten start ',
            payloadId: 'thinking:think:0:0',
            updatedAt: 4,
          }),
        );
        // Already invalid at read time before any delta arrives.
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        pane.applyItemDelta({
          threadId: 'thread-stale-reseed',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'next ',
          updatedAt: 5,
        });
        safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('rewritten start next ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('retains the tail when a caught-up smoother settles via terminal upsert', async () => {
      // Terminal upserts (upsertItemsBatch reconcile) are the third
      // settle path next to the summary-carrying and bare-status
      // patches; a caught-up reveal must retain its tail there too.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-upsert-settle' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-upsert-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-upsert-settle',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'upsert settle reasoning ',
          updatedAt: 2,
        });
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-upsert-settle',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: pane.items[0].summary,
            payloadId: 'thinking:think:0:0',
            updatedAt: 3,
          }),
        );
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe('upsert settle reasoning ');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drains a same-stream reasoning tail through a terminal upsert', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-upsert-drain' }));
        const item = makeItem({
          id: 'think:0:0',
          threadId: 'thread-upsert-drain',
          kind: 'thinking',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          payloadId: 'thinking:think:0:0',
          updatedAt: 1,
        });
        pane.upsertItem(item);
        const fullText = Array.from(
          { length: 120 },
          (_, index) => `reason${String(index).padStart(3, '0')} `,
        ).join('');
        pane.applyItemDelta({
          threadId: item.threadId,
          itemId: item.id,
          kind: item.kind,
          delta: fullText,
          updatedAt: 2,
        });
        for (let frame = 0; frame < 4; frame++) clock.tickFrame(16);
        const visibleAtCompletion = pane.items[0].summary;
        expect(visibleAtCompletion.length).toBeGreaterThan(0);
        expect(visibleAtCompletion.length).toBeLessThan(fullText.length);

        pane.upsertItem({
          ...pane.items[0],
          status: 'completed',
          summary: fullText.slice(-400),
          updatedAt: 3,
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe(visibleAtCompletion);
        expect(pane.isItemSmoothing(item.id)).toBe(true);
        let safety = 1_000;
        while (pane.isItemSmoothing(item.id) && safety-- > 0) {
          clock.tickFrame(16);
        }
        expect(safety).toBeGreaterThan(0);
        expect(pane.items[0].summary).toBe(fullText.slice(-400));
        expect(pane.liveThinkingTailForItem(item.id)).toBe(fullText);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the tail when a mid-drain smoother is replaced by a terminal upsert', async () => {
      // A mid-drain smoother's tail is partial — it can never match a
      // terminal summary, so the reconcile disposes rather than retains.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-middrain-upsert' }));
        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-middrain-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            payloadId: 'thinking:think:0:0',
            updatedAt: 1,
          }),
        );
        const words: string[] = [];
        for (let i = 0; i < 80; i++) words.push(`tok${String(i).padStart(2, '0')}`);
        pane.applyItemDelta({
          threadId: 'thread-middrain-upsert',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: words.join(' ') + ' ',
          updatedAt: 2,
        });
        // A few frames: enough for a partial reveal (the tail entry
        // exists), far from caught up.
        for (let i = 0; i < 3; i++) clock.tickFrame(16);
        expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

        pane.upsertItem(
          makeItem({
            id: 'think:0:0',
            threadId: 'thread-middrain-upsert',
            kind: 'thinking',
            role: 'assistant',
            status: 'completed',
            summary: 'final from upsert',
            payloadId: 'thinking:think:0:0',
            updatedAt: 3,
          }),
        );
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('evicts the oldest settled tails past the retained-char budget', async () => {
      // The offscreen prune only runs while a MessageTimeline is
      // mounted; a backgrounded pane (Settings replaces the surface)
      // keeps settling rows with no prune cadence. The store-side char
      // budget is the backstop: oldest settled tails evict once the
      // total passes SETTLED_TAIL_BUDGET_CHARS (131072).
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-tail-budget' }));
        const tailFor = (i: number): string => `row${i} ` + 'x'.repeat(59_995);
        for (const i of [0, 1, 2]) {
          pane.upsertItem(
            makeItem({
              id: `think:0:${i}`,
              threadId: 'thread-tail-budget',
              turnIndex: 0,
              itemIndex: i,
              kind: 'thinking',
              role: 'assistant',
              status: 'streaming',
              summary: '',
              payloadId: `thinking:think:0:${i}`,
              updatedAt: 1,
            }),
          );
          pane.applyItemDelta({
            threadId: 'thread-tail-budget',
            itemId: `think:0:${i}`,
            kind: 'thinking',
            delta: tailFor(i),
            updatedAt: 2,
          });
        }
        // Snap + settle all three in insertion order; the third settle
        // pushes the total to ~180k and evicts the oldest back under
        // budget.
        pane.__flushItemSmoothersForTest();
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:1')).toBe(tailFor(1));
        expect(pane.liveThinkingTailForItem('think:0:2')).toBe(tailFor(2));
        expect(pane.debugMemoryStats().liveThinkingTailChars).toBe(tailFor(1).length * 2);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('disposes the smoother when a completed patch re-asserts the equal summary (Codex assistant_text)', async () => {
      // The sibling of the bare-status leak test. Codex content-block-stop
      // carries ContentPresent=true, so doSettleStreamingText re-asserts
      // the full summary on the completion patch. When that summary equals
      // what the smoother already received AND the smoother is caught up,
      // the extend/snap branches are both skipped (summary === received)
      // and the bare-status dispose branch is unreachable (it is an
      // else-if after the summary branch). Before the fix the smoother
      // leaked until the next thread switch. assistant_text has no
      // live-tail observable, so assert on the smoother count directly.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-equal-text' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-equal-text',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-equal-text',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'hello world ',
          updatedAt: 2,
        });
        // Drain to caught-up so the onReveal auto-cleanup can't fire later.
        let safety = 500;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        pane.applyItemPatch({
          threadId: 'thread-equal-text',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: 'hello world ', updatedAt: 3 },
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe('hello world ');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        // No zombie rAF ticks left behind.
        safety = 20;
        while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('preserves the full revealed text when a snap-status patch omits a summary', async () => {
      // Regression for the dead-snap discard: the isSnapStatus branch
      // snaps the smoother (writing the full received text into
      // items[index] via onReveal), but the final item was rebuilt from
      // the PRE-snap `current` capture — discarding that write. A
      // kill/error patch that carries no summary would then revert to the
      // partial pre-snap text, losing the already-streamed tail. The fix
      // rebuilds from items[index] so the snap survives.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-snap-nosum' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-snap-nosum',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'partial so far',
            updatedAt: 1,
          }),
        );
        // Append a tail the smoother has received but NOT yet revealed
        // (no clock ticks fired), so snap has real work to do.
        pane.applyItemDelta({
          threadId: 'thread-snap-nosum',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: ' and then more',
          updatedAt: 2,
        });
        // Kill with status only — no summary in the patch.
        pane.applyItemPatch({
          threadId: 'thread-snap-nosum',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'killed', updatedAt: 3 },
        });
        expect(pane.items[0].status).toBe('killed');
        // The snap revealed everything; the no-summary patch must keep it.
        expect(pane.items[0].summary).toBe('partial so far and then more');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });

  describe('visibility-resume snap (snapSmoothersToReceived)', () => {
    // requestAnimationFrame is suspended while a tab is hidden, but the
    // WebSocket keeps delivering deltas into each smoother's `received`
    // buffer. The FakeSmoothingClock models this exactly: appending a delta
    // without calling `tickFrame` leaves `received` ahead of `revealed` with
    // a pending callback that never fires — the hidden-tab state. The
    // visibilitychange→visible entry point (App.svelte) calls
    // `snapSmoothersToReceived` so the backlog catches up to the wire in one
    // frame instead of crawling in at MAX_ADAPTIVE_CHARS_PER_SEC on return.
    function manyWords(prefix: string, n: number): string {
      return Array.from(
        { length: n },
        (_, i) => `${prefix}${String(i).padStart(2, '0')}`,
      ).join(' ');
    }

    it('snaps a backlogged STILL-STREAMING row to the wire and keeps the smoother live', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-a' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-a',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        // ~700 chars in one delta — far more than one tick's 14-char cap.
        const big = manyWords('word', 100);
        pane.applyItemDelta({
          threadId: 'thread-vis-a',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: big,
          updatedAt: 2,
        });
        // Hidden: nothing revealed yet, a pending rAF would crawl it in.
        expect(pane.items[0].summary).toBe('');
        expect(clock.pendingCount()).toBeGreaterThan(0);

        pane.snapSmoothersToReceived();

        // Caught up to the wire in one call; the pending rAF is canceled.
        expect(pane.items[0].summary).toBe(big);
        expect(clock.pendingCount()).toBe(0);
        // Row is still streaming, so the smoother is retained for the rest
        // of the live turn rather than disposed.
        expect(pane.items[0].status).toBe('streaming');
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // A later delta still animates — snap leaves the smoother usable.
        pane.applyItemDelta({
          threadId: 'thread-vis-a',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: ' more',
          updatedAt: 3,
        });
        expect(pane.items[0].summary).toBe(big); // not revealed until a tick
        for (let frame = 0; clock.pendingCount() > 0 && frame < 100; frame++) {
          clock.tickFrame(16);
        }
        expect(clock.pendingCount()).toBe(0);
        expect(pane.items[0].summary).toBe(`${big} more`);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('snaps and disposes a row that COMPLETED while hidden instead of crawling on return', async () => {
      // The headline regression. The row streams AND completes in the
      // background; the completion patch re-asserts the equal summary (Codex
      // content-block-stop shape) but the smoother is still backlogged, so
      // none of applyItemPatch's dispose branches fire (summary === received,
      // not caught up). Without the visibility snap the finished response
      // would type itself in at the per-tick cap when the tab regains focus.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-b' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-b',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        const full = manyWords('tok', 100);
        pane.applyItemDelta({
          threadId: 'thread-vis-b',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: full,
          updatedAt: 2,
        });
        pane.applyItemPatch({
          threadId: 'thread-vis-b',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: full, updatedAt: 3 },
        });
        // Bug shape on return WITHOUT the snap: status is completed but the
        // text has not been revealed, and a pending rAF would drain it slowly.
        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe('');
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(clock.pendingCount()).toBeGreaterThan(0);

        pane.snapSmoothersToReceived();

        // Fully shown in one frame; the terminal-status onReveal cleanup
        // disposes the smoother; no lingering rAF to crawl the text in.
        expect(pane.items[0].summary).toBe(full);
        expect(pane.items[0].status).toBe('completed');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('is a no-op when smoothers are caught up or absent', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-vis-c' }));
        // No smoothers yet: safe to call.
        expect(() => pane.snapSmoothersToReceived()).not.toThrow();

        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 'thread-vis-c',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 'thread-vis-c',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'short text ',
          updatedAt: 2,
        });
        // Fully drain so the smoother is caught up but still streaming.
        while (clock.pendingCount() > 0) clock.tickFrame(16);
        expect(pane.items[0].summary).toBe('short text ');
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        // A caught-up snap changes nothing and keeps the streaming smoother.
        pane.snapSmoothersToReceived();
        expect(pane.items[0].summary).toBe('short text ');
        expect(pane.items[0].status).toBe('streaming');
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        expect(clock.pendingCount()).toBe(0);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });
});
