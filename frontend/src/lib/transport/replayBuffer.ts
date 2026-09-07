import { MAX_REPLAY_CHANNELS, type ServerEventFrame } from './frames';

type ReplayEvent = Omit<ServerEventFrame, 'type'>;
export const MAX_REPLAY_BUFFER_EVENTS = 4096;
export const MAX_REPLAY_BUFFER_CHARS = 4 * 1024 * 1024;

/** A reconnect may receive live events before or between replay batches.
 * Keep each channel ordered without changing the cross-channel slot order.
 * On overflow retain only channel heads: callers recover those by snapshot. */
export class ReplayBuffer {
  private events: ReplayEvent[] = [];
  private chars = 0;
  private overflow = false;
  private heads = new Map<string, number>();
  private headChars = 0;

  addFrameSize(chars: number): void {
    if (this.overflow) return;
    this.chars += chars;
    if (this.chars > MAX_REPLAY_BUFFER_CHARS) this.discardPayloads();
  }

  push(event: ReplayEvent): void {
    const previous = this.heads.get(event.channel);
    if (previous !== undefined) {
      this.heads.set(event.channel, Math.max(previous, event.seq));
    } else if (this.heads.size < MAX_REPLAY_CHANNELS
      && this.headChars + event.channel.length <= MAX_REPLAY_BUFFER_CHARS) {
      this.heads.set(event.channel, event.seq);
      this.headChars += event.channel.length;
    } else {
      // A peer can keep streaming novel channels after payload overflow.
      // Bound recovery metadata too; callers also recover their known channels.
      this.discardPayloads();
    }
    if (this.overflow) return;
    if (this.events.length === MAX_REPLAY_BUFFER_EVENTS) {
      this.discardPayloads();
      return;
    }
    this.events.push(event);
  }

  private discardPayloads(): void {
    if (this.overflow) return;
    this.overflow = true;
    this.events = [];
  }

  drain(deliver: (event: ReplayEvent) => void, recover: (heads: ReadonlyMap<string, number>) => void): void {
    if (this.overflow) {
      recover(this.heads);
      return;
    }
    const channels = new Map<string, { events: ReplayEvent[]; next: number }>();
    for (const event of this.events) {
      let queue = channels.get(event.channel);
      if (!queue) { queue = { events: [], next: 0 }; channels.set(event.channel, queue); }
      queue.events.push(event);
    }
    for (const queue of channels.values()) {
      queue.events.sort((a, b) => a.seq - b.seq || Number(b.gap === true) - Number(a.gap === true));
    }
    for (const slot of this.events) {
      const queue = channels.get(slot.channel)!;
      deliver(queue.events[queue.next++]!);
    }
  }
}
