// Shared discussion-channel test factories (mirrors helpers/chat.ts).
// Canonical defaults for the ChannelMessage / ChannelStatePayload wire
// shapes; suites layer their own file-specific defaults via thin local
// wrappers and per-call overrides.
import type { ChannelMessage, ChannelStatePayload } from '../../lib/types/discussion';

export function makeChannelMessage(overrides: Partial<ChannelMessage> = {}): ChannelMessage {
  return {
    id: 'message-' + (overrides.sequence ?? 1),
    channelId: 'channel-1',
    sequence: 1,
    fromType: 'agent',
    fromId: 'advocate-thread',
    fromRole: 'advocate',
    content: 'hello',
    createdAt: 0,
    ...overrides,
  };
}

export function makeChannelStatePayload(
  overrides: Partial<ChannelStatePayload> = {},
): ChannelStatePayload {
  return {
    channelId: 'channel-1',
    threadId: 'parent-thread',
    status: 'open',
    turnCount: 0,
    maxTurns: 8,
    awaitingResponse: false,
    currentSpeakerThreadId: 'advocate-thread',
    currentSpeakerRole: 'advocate',
    participants: [
      { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: false },
      { threadId: 'critic-thread', role: 'critic', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: false },
    ],
    ...overrides,
  };
}
