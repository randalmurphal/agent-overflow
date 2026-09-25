// Per-client wire accounting for the cost specs (agent-cost.spec.ts,
// stop-scale.spec.ts). A recorder attaches to a page's WebSockets and
// counts, while armed, what the backend delivered to that page: event
// frames and their bytes by channel, split by whether the event names a
// given thread, and the RPCs the page sent with the bytes of their replies.
// A coalesced window arrives as one `batch` frame, so batches are unpacked
// and each event is charged the bytes of its own JSON.
import type { Page } from '@playwright/test';
import { methodNameById } from './offhost-helpers.js';

export interface ChannelTally {
  events: number;
  bytes: number;
}

export interface WireTally {
  /** Every event delivered, by channel. */
  channels: Record<string, ChannelTally>;
  /** Events whose payload names `threadId`, by channel. */
  threadChannels: Record<string, ChannelTally>;
  /** RPC requests the page sent, by method. */
  rpcCalls: Record<string, number>;
  /** Bytes of the RPC replies the page received, by method. */
  rpcReplyBytes: Record<string, number>;
  /** Every byte the page received while armed. */
  totalBytes: number;
}

export interface WireRecorder {
  /** Start counting from zero, charging `threadId`'s events separately. */
  arm(threadId: string): void;
  /** Stop counting and return what was counted. */
  stop(): WireTally;
  /** Resolve at the next event on `channel` that `match` accepts. */
  awaitEvent(channel: string, match: (data: unknown) => boolean, timeoutMs?: number): Promise<void>;
}

function emptyTally(): WireTally {
  return { channels: {}, threadChannels: {}, rpcCalls: {}, rpcReplyBytes: {}, totalBytes: 0 };
}

function add(table: Record<string, ChannelTally>, channel: string, bytes: number): void {
  const row = (table[channel] ??= { events: 0, bytes: 0 });
  row.events += 1;
  row.bytes += bytes;
}

function namesThread(data: unknown, threadId: string): boolean {
  if (!data || typeof data !== 'object') return false;
  const record = data as Record<string, unknown>;
  if (record.threadId === threadId) return true;
  const item = record.item as Record<string, unknown> | undefined;
  return item?.threadId === threadId;
}

/** Attach a recorder to every WebSocket the page opens from now on. */
export function recordPageWire(page: Page): WireRecorder {
  let tally: WireTally | null = null;
  let threadId = '';
  const waiters = new Set<(channel: string, data: unknown) => void>();
  page.on('websocket', (ws) => {
    const methodById = new Map<string, string>();
    ws.on('framesent', (frame) => {
      if (!tally) return;
      let parsed: { type?: string; id?: string; method?: string; methodId?: number };
      try {
        parsed = JSON.parse(String(frame.payload)) as typeof parsed;
      } catch {
        return;
      }
      if (parsed.type !== 'rpc' || !parsed.id) return;
      const name = parsed.method ?? (parsed.methodId ? methodNameById(parsed.methodId) : 'unknown');
      methodById.set(parsed.id, name);
      tally.rpcCalls[name] = (tally.rpcCalls[name] ?? 0) + 1;
    });
    ws.on('framereceived', (frame) => {
      if (!tally) return;
      const text = String(frame.payload);
      tally.totalBytes += text.length;
      let parsed: {
        type?: string;
        id?: string;
        channel?: string;
        data?: unknown;
        events?: Array<{ channel?: string; data?: unknown }>;
      };
      try {
        parsed = JSON.parse(text) as typeof parsed;
      } catch {
        return;
      }
      const charge = (channel: string | undefined, data: unknown, bytes: number) => {
        if (!channel || !tally) return;
        add(tally.channels, channel, bytes);
        if (namesThread(data, threadId)) add(tally.threadChannels, channel, bytes);
        for (const waiter of [...waiters]) waiter(channel, data);
      };
      if (parsed.type === 'event') {
        charge(parsed.channel, parsed.data, text.length);
        return;
      }
      if (parsed.type === 'batch') {
        for (const entry of parsed.events ?? []) {
          charge(entry.channel, entry.data, JSON.stringify(entry).length);
        }
        return;
      }
      if (parsed.type === 'rpc' && parsed.id) {
        const name = methodById.get(parsed.id) ?? 'unknown';
        methodById.delete(parsed.id);
        tally.rpcReplyBytes[name] = (tally.rpcReplyBytes[name] ?? 0) + text.length;
      }
    });
  });
  return {
    arm(id: string) {
      threadId = id;
      tally = emptyTally();
    },
    stop(): WireTally {
      const out = tally ?? emptyTally();
      tally = null;
      return out;
    },
    awaitEvent(channel: string, match: (data: unknown) => boolean, timeoutMs = 30_000): Promise<void> {
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          waiters.delete(waiter);
          reject(new Error(`no ${channel} event reached the page within ${timeoutMs}ms`));
        }, timeoutMs);
        const waiter = (got: string, data: unknown) => {
          if (got !== channel || !match(data)) return;
          clearTimeout(timer);
          waiters.delete(waiter);
          resolve();
        };
        waiters.add(waiter);
      });
    },
  };
}

/** Sum of the events and bytes over the named channels (every channel when omitted). */
export function sumChannels(table: Record<string, ChannelTally>, channels?: string[]): ChannelTally {
  const out = { events: 0, bytes: 0 };
  for (const [channel, row] of Object.entries(table)) {
    if (channels && !channels.includes(channel)) continue;
    out.events += row.events;
    out.bytes += row.bytes;
  }
  return out;
}
