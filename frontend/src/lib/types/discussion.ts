export interface DiscussionParticipant {
  role: string;
  description: string;
  system: string;
  provider?: string;
  model?: string;
}

export interface DiscussionDefinition {
  id: string;
  name: string;
  description: string;
  scope: 'global' | 'project';
  projectId?: string;
  participants: DiscussionParticipant[];
  settings: { maxTurns: number };
  createdAt: number;
  updatedAt: number;
}

export interface Channel {
  id: string;
  threadId: string;
  type: string;
  status: 'open' | 'concluded' | 'closed';
  createdAt: number;
  updatedAt: number;
}

export interface ChannelMessage {
  id: string;
  channelId: string;
  sequence: number;
  fromType: 'human' | 'agent';
  fromId: string;
  fromRole?: string;
  content: string;
  createdAt: number;
}

export interface DeliberationState {
  channelId: string;
  currentSpeaker: string;
  turnCount: number;
  maxTurns: number;
  conclusionProposals: Record<string, string>;
  concluded: boolean;
}
