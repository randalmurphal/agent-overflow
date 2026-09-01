// Socket recording for the per-thread subscription narrowing specs
// (docs/specs/remote-access.md §9).
//
// The socket is recorded in both directions by replacing `WebSocket` in an
// init script. Test-side only, deliberately: an observation hook in the
// transport would be production code that exists for one spec family, and
// the property under test is precisely what goes over the wire.
//
// Shared by transport-watch-narrowing.spec.ts (the frame the client sends)
// and transport-watch-badge-carriers.spec.ts (the frames it is answered
// with), because both need the SAME recorder on more than one page.
import type { Page } from '@playwright/test';

/** One frame the page sent, in send order. */
export interface SentFrame {
  type: string;
  /** Present on watch frames. */
  threads?: string[];
  /** The raw JSON, for "does this frame name that thread" questions. */
  text: string;
}

/** One event the page received, in arrival order. */
export interface ReceivedEvent {
  channel: string;
  threadId: string;
}

export interface WireLog {
  sent: SentFrame[];
  received: ReceivedEvent[];
}

/**
 * Replace the page's WebSocket with a recording subclass. Must run before
 * the bundle constructs its client, which is what addInitScript guarantees.
 */
export async function recordWire(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const scope = window as unknown as { __aoWire?: WireLog; WebSocket: typeof WebSocket };
    const log: WireLog = { sent: [], received: [] };
    scope.__aoWire = log;

    const note = (frame: Record<string, unknown>) => {
      if (frame.type === 'event') {
        // `threadId` is the entity key nearly every per-thread channel
        // carries. `thread:updated` — the wildcard carrier the badge spec
        // reads — names its subject three ways depending on the action:
        // `id` on a patch, `thread.id` on a full row. Each fallback only
        // applies when the one before it is absent, so no channel that
        // carries `threadId` is ever mis-keyed.
        const data = (frame.data ?? {}) as Record<string, unknown>;
        const thread = (data.thread ?? {}) as Record<string, unknown>;
        log.received.push({
          channel: String(frame.channel ?? ''),
          threadId: String(data.threadId ?? data.id ?? thread.id ?? ''),
        });
      } else if (frame.type === 'batch' && Array.isArray(frame.events)) {
        for (const entry of frame.events as Array<Record<string, unknown>>) note(entry);
      }
    };

    const Base = scope.WebSocket;
    class RecordingWebSocket extends Base {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        this.addEventListener('message', (event: MessageEvent) => {
          if (typeof event.data !== 'string') return;
          try {
            note(JSON.parse(event.data) as Record<string, unknown>);
          } catch {
            // Not a frame this spec reads about; the app still gets it.
          }
        });
      }

      override send(data: Parameters<WebSocket['send']>[0]): void {
        if (typeof data === 'string') {
          try {
            const frame = JSON.parse(data) as Record<string, unknown>;
            log.sent.push({
              type: String(frame.type ?? ''),
              threads: Array.isArray(frame.threads) ? (frame.threads as string[]) : undefined,
              text: data,
            });
          } catch {
            // Same: record what parses, forward everything.
          }
        }
        super.send(data);
      }
    }
    scope.WebSocket = RecordingWebSocket as unknown as typeof WebSocket;
  });
}

export function readWire(page: Page): Promise<WireLog> {
  return page.evaluate(() => (window as unknown as { __aoWire: WireLog }).__aoWire);
}

/** The threads named by the most recent watch frame, or null if none sent. */
export function watchedNow(wire: WireLog): string[] | null {
  const frames = wire.sent.filter((frame) => frame.type === 'watch');
  const last = frames.at(-1);
  return last ? [...(last.threads ?? [])].sort() : null;
}

/** Every thread id this page was pushed a frame for on `channel`, in arrival order. */
export function receivedOn(wire: WireLog, channel: string): string[] {
  return wire.received.filter((event) => event.channel === channel).map((event) => event.threadId);
}
