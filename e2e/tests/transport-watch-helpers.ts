// Socket recording for the per-thread subscription narrowing specs
// (docs/specs/remote-access.md §9).
//
// The socket is recorded in both directions by replacing `WebSocket` in an
// init script. Test-side only, deliberately: an observation hook in the
// transport would be production code that exists for one spec family, and
// the property under test is precisely what goes over the wire.
//
// Shared by transport-watch-narrowing.spec.ts (the frame the client sends),
// transport-watch-badge-carriers.spec.ts (the frames it is answered with),
// transport-watch-scopes.spec.ts (the subagent rows a scope admits) and
// transport-watermark.spec.ts (the cursor a reconnect asks from), because
// they need the SAME recorder, some on more than one page.
import type { Page } from '@playwright/test';

/** One frame the page sent, in send order. */
export interface SentFrame {
  type: string;
  /** Present on watch frames. */
  threads?: string[];
  /** Present on watch frames that state a scope set. */
  scopes?: Array<{ threadId: string; scopeRootId: string }>;
  /** The raw JSON, for "does this frame name that thread" questions. */
  text: string;
}

/** One event the page received, in arrival order. */
export interface ReceivedEvent {
  channel: string;
  seq: number;
  threadId: string;
  /**
   * `provider:item_event` only: which action the frame carried. The lease
   * spec counts `delta` frames, which is the only question that needs to
   * distinguish one item_event from another; every other spec ignores it.
   */
  action?: string;
  /** `provider:item_event` only: the row, and its parent (empty for a root row). */
  itemId?: string;
  parentId?: string;
  /** The event's JSON length, the share of the wire it cost. */
  bytes: number;
}

/** One watermark the page received: a cursor-only event, with no data. */
export interface ReceivedWatermark {
  channel: string;
  seq: number;
  /** Index into `WireLog.sockets` of the socket it arrived on. */
  socket: number;
}

export interface WireLog {
  sent: SentFrame[];
  /** Events with data. Watermarks are kept apart, in `watermarks`. */
  received: ReceivedEvent[];
  watermarks: ReceivedWatermark[];
  /** Each socket's hello `replayBaseline`, in socket order. */
  hellos: Array<Record<string, number>>;
  /** `sent.length` when each socket was constructed, one per connection. */
  sockets: number[];
  /** Ids of the reply frames the page received, in arrival order. */
  replies: string[];
  /** Text bytes and messages received, over every socket. */
  receivedBytes: number;
  receivedMessages: number;
}

/**
 * Replace the page's WebSocket with a recording subclass. Must run before
 * the bundle constructs its client, which is what addInitScript guarantees.
 */
export async function recordWire(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const scope = window as unknown as {
      __aoWire?: WireLog;
      __aoWireSocket?: WebSocket;
      WebSocket: typeof WebSocket;
    };
    const log: WireLog = {
      sent: [], received: [], watermarks: [], hellos: [], sockets: [], replies: [], receivedBytes: 0, receivedMessages: 0,
    };
    scope.__aoWire = log;

    const note = (frame: Record<string, unknown>, socket: number) => {
      if (frame.type === 'event' && frame.watermark === true) {
        log.watermarks.push({ channel: String(frame.channel ?? ''), seq: Number(frame.seq), socket });
      } else if (frame.type === 'event') {
        // `threadId` is the entity key nearly every per-thread channel
        // carries. `thread:updated` — the wildcard carrier the badge spec
        // reads — names its subject three ways depending on the action:
        // `id` on a patch, `thread.id` on a full row. Each fallback only
        // applies when the one before it is absent, so no channel that
        // carries `threadId` is ever mis-keyed.
        const data = (frame.data ?? {}) as Record<string, unknown>;
        const thread = (data.thread ?? {}) as Record<string, unknown>;
        const entry: ReceivedEvent = {
          channel: String(frame.channel ?? ''),
          seq: Number(frame.seq),
          threadId: String(data.threadId ?? data.id ?? thread.id ?? ''),
          bytes: JSON.stringify(frame).length,
        };
        if (typeof data.action === 'string') entry.action = data.action;
        if (entry.channel === 'provider:item_event') {
          // Every item event names its row's parent: deltas, metas and
          // patches at the top level, upserts on the row.
          const item = (data.item ?? {}) as Record<string, unknown>;
          entry.itemId = String(data.itemId ?? item.id ?? '');
          entry.parentId = String(data.parentId ?? item.parentId ?? '');
        }
        log.received.push(entry);
      } else if (frame.type === 'batch' && Array.isArray(frame.events)) {
        for (const entry of frame.events as Array<Record<string, unknown>>) note(entry, socket);
      } else if (frame.type === 'hello') {
        log.hellos.push({ ...((frame.replayBaseline ?? {}) as Record<string, number>) });
      } else if (typeof frame.id === 'string' && frame.id !== '') {
        log.replies.push(frame.id);
      }
    };

    const Base = scope.WebSocket;
    class RecordingWebSocket extends Base {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        const socket = log.sockets.length;
        log.sockets.push(log.sent.length);
        // The page's live socket, so a spec can put a frame on the wire the
        // SPA has no caller for yet. The lease spec is the one user: the
        // frame's producer is a native shell that does not exist in a
        // browser, so driving it any other way would be testing a stub.
        scope.__aoWireSocket = this as unknown as WebSocket;
        this.addEventListener('message', (event: MessageEvent) => {
          if (typeof event.data !== 'string') return;
          log.receivedBytes += event.data.length;
          log.receivedMessages += 1;
          try {
            note(JSON.parse(event.data) as Record<string, unknown>, socket);
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
              scopes: Array.isArray(frame.scopes)
                ? (frame.scopes as Array<{ threadId: string; scopeRootId: string }>)
                : undefined,
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

/**
 * Write one client frame on the page's own live socket.
 *
 * For frames the SPA has no caller for. The transport module exposes the
 * lease as `setClientLease`, but its producer is a native app-lifecycle
 * plugin that no browser has, so a spec that reached for a stub would be
 * proving the stub. This puts the real bytes on the real connection and
 * lets the backend answer.
 */
export async function sendClientFrame(page: Page, frame: Record<string, unknown>): Promise<void> {
  await page.evaluate((text) => {
    const socket = (window as unknown as { __aoWireSocket?: WebSocket }).__aoWireSocket;
    if (!socket || socket.readyState !== socket.OPEN) throw new Error('no open socket to write on');
    socket.send(text);
  }, JSON.stringify(frame));
}

/** The threads named by the most recent watch frame, or null if none sent. */
export function watchedNow(wire: WireLog): string[] | null {
  const frames = wire.sent.filter((frame) => frame.type === 'watch');
  const last = frames.at(-1);
  return last ? [...(last.threads ?? [])].sort() : null;
}

/**
 * The `thread/scopeRoot` pairs named by the most recent watch frame, sorted;
 * null if no watch was sent or the last one stated no scope set.
 */
export function watchedScopesNow(wire: WireLog): string[] | null {
  const last = wire.sent.filter((frame) => frame.type === 'watch').at(-1);
  if (!last?.scopes) return null;
  return last.scopes.map((scope) => `${scope.threadId}/${scope.scopeRootId}`).sort();
}

/**
 * Wait until the backend has handled every frame this page sent so far.
 *
 * The backend reads one connection's frames in order on one loop, and a
 * watch is applied before the next frame is read, so the reply to a call
 * sent now proves every earlier watch is in force. The call names no
 * method, which the backend answers with an error and the page ignores.
 */
export async function fenceSocket(page: Page, id: string): Promise<void> {
  await sendClientFrame(page, { type: 'rpc', id, method: 'TransportWatchFence', params: [] });
  await page.waitForFunction(
    (fence) => (window as unknown as { __aoWire: WireLog }).__aoWire.replies.includes(fence),
    id,
  );
}

/** Every thread id this page was pushed a frame for on `channel`, in arrival order. */
export function receivedOn(wire: WireLog, channel: string): string[] {
  return wire.received.filter((event) => event.channel === channel).map((event) => event.threadId);
}
