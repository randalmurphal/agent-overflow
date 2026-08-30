// stores/threadPaneErrors.svelte.ts
//
// OWNS the pane's user-facing error surface: one slot per `PaneErrorKind`,
// the write order that ranks them, and the fixed banner-stack order they
// render in. Every write and every clear in the pane goes through this
// module, which is the whole point — the untagged writer used to share a
// slot with the retryable ones and destroyed a live Retry button.
//
// MUST NOT know what any error MEANS. It never inspects a message, never
// decides which action a banner offers (the component maps kind → action),
// and holds nothing about the thread, the provider or the wire.
// `providerBanner` is a separate, independent reason to show the banner and
// stays on the pane.

import type { PaneErrorKind } from './threadPaneShared';

/** One stored error: the message plus the write order that ranks it. */
interface PaneErrorEntry {
  readonly message: string;
  readonly seq: number;
}

/** Shared empty map so `clear()` on a clean pane is identity-stable. */
const EMPTY_PANE_ERRORS: Readonly<Partial<Record<PaneErrorKind, PaneErrorEntry>>> =
  Object.freeze({});

/** Every kind the surface can hold — iteration order is not the ranking. */
const PANE_ERROR_KINDS: readonly PaneErrorKind[] = Object.freeze([
  'general',
  'session',
  'history-load',
]);

/** Top-to-bottom order of the stacked banner rows; see `list`. */
const PANE_ERROR_DISPLAY_ORDER: readonly PaneErrorKind[] = Object.freeze([
  'session',
  'history-load',
  'general',
]);

export interface ThreadPaneErrors {
  /**
   * The ONE error-writing entry point. `kind` decides which slot the
   * message occupies and which action the banner offers; a second write
   * of the same kind replaces that kind's message and nothing else.
   */
  set(message: string, kind?: PaneErrorKind): void;
  /** Clear one kind, or every kind when `kind` is omitted. */
  clear(kind?: PaneErrorKind): void;
  /**
   * Every stored error, in the fixed order the banner stack renders them:
   * session (Reconnect) on top, then history-load (Retry), then general.
   * Fixed by kind rather than by write order so rows never reshuffle
   * under the pointer when a second error lands.
   */
  list(): { kind: PaneErrorKind; message: string }[];
  /**
   * The newest stored error — the single-error convenience read behind
   * `generalError`/`generalErrorKind`. Display goes through `list`; this
   * exists for presence checks and scope views.
   */
  newest(): { message: string; kind: PaneErrorKind } | null;
}

/**
 * The pane's user-facing error state, surfaced by ProviderStatusBanner
 * for non-wire failures: thread load failures, composer send failures,
 * git action failures, reconnect failures. Deliberately distinct from
 * `providerBanner` (which mirrors the provider's own session/auth/
 * rate-limit state) — consumers treat them as two independent reasons
 * to show the top-of-pane banner.
 *
 * Stored PER KIND rather than in one slot. There used to be four
 * writers each assigning the same pair of variables, and the untagged
 * one (`setGeneralError`, ~15 call sites: rename failed, git action
 * failed, queue failed, workspace prep failed, …) destroyed a live
 * retryable `history-load` banner along with its Retry button. Now
 * every write and every clear goes through `set` / `clear`:
 *
 *   - `session`      — a provider session_died event; carries Reconnect.
 *   - `history-load` — the initial history window failed and can be
 *                      retried in place; carries Retry.
 *   - `general`      — everything else; carries no action.
 *
 * Every stored kind RENDERS, as its own stacked banner row with its
 * own action and its own Dismiss (`list`, user ruling 2026-08-25 —
 * this replaced the earlier one-slot resolution rule, whose no-clobber
 * exception silently hid a general error for as long as a history-load
 * banner was up). A second write of the same kind replaces that kind's
 * message; kinds never displace each other.
 */
export function createThreadPaneErrors(): ThreadPaneErrors {
  let paneErrors: Readonly<Partial<Record<PaneErrorKind, PaneErrorEntry>>> =
    $state.raw(EMPTY_PANE_ERRORS);
  let paneErrorWriteSeq = 0;

  return {
    set(message: string, kind: PaneErrorKind = 'general'): void {
      paneErrors = { ...paneErrors, [kind]: { message, seq: ++paneErrorWriteSeq } };
    },
    clear(kind?: PaneErrorKind): void {
      if (kind === undefined) {
        if (paneErrors === EMPTY_PANE_ERRORS) return;
        paneErrors = EMPTY_PANE_ERRORS;
        return;
      }
      if (paneErrors[kind] === undefined) return;
      const next = { ...paneErrors };
      delete next[kind];
      paneErrors = next;
    },
    list(): { kind: PaneErrorKind; message: string }[] {
      const out: { kind: PaneErrorKind; message: string }[] = [];
      for (const kind of PANE_ERROR_DISPLAY_ORDER) {
        const entry = paneErrors[kind];
        if (entry !== undefined) out.push({ kind, message: entry.message });
      }
      return out;
    },
    newest(): { message: string; kind: PaneErrorKind } | null {
      let best: PaneErrorEntry | undefined;
      let bestKind: PaneErrorKind | null = null;
      for (const kind of PANE_ERROR_KINDS) {
        const entry = paneErrors[kind];
        if (entry === undefined) continue;
        if (best === undefined || entry.seq > best.seq) {
          best = entry;
          bestKind = kind;
        }
      }
      return best === undefined || bestKind === null
        ? null
        : { message: best.message, kind: bestKind };
    },
  };
}
