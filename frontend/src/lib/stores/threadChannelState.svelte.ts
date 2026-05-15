import type { ChannelMessage } from '../types/discussion';

export type ThreadChannelStatus = 'open' | 'concluded' | 'closed' | null;

export interface ThreadChannelState {
  readonly messages: ChannelMessage[];
  readonly status: ThreadChannelStatus;
  mergeMessages(incoming: ChannelMessage[]): void;
  setStatus(status: ThreadChannelStatus): void;
  clear(): void;
}

export function createThreadChannelState(): ThreadChannelState {
  let messages: ChannelMessage[] = $state([]);
  let status: ThreadChannelStatus = $state(null);

  /**
   * Merge channel messages into local state, de-duplicating by sequence.
   * Expected to be called with `afterSeq` set to the highest sequence we've
   * seen, so most calls append a small number of rows.
   */
  function mergeMessages(incoming: ChannelMessage[]): void {
    if (!incoming || incoming.length === 0) return;

    const seenSequences = new Set(messages.map((message) => message.sequence));
    const nextMessages = messages.slice();
    for (const message of incoming) {
      if (seenSequences.has(message.sequence)) continue;
      nextMessages.push(message);
      seenSequences.add(message.sequence);
    }
    nextMessages.sort((a, b) => a.sequence - b.sequence);
    messages = nextMessages;
  }

  function setStatus(nextStatus: ThreadChannelStatus): void {
    status = nextStatus;
  }

  function clear(): void {
    messages = [];
    status = null;
  }

  return {
    get messages() { return messages; },
    get status() { return status; },
    mergeMessages,
    setStatus,
    clear,
  };
}
