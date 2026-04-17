export interface TerminalChip {
  id: string;
  label: string;
  preview: string;
  content: string;
  createdAt: number;
}

export interface Draft {
  threadId: string;
  content: string;
  attachmentIds: string[];
  terminalChips: TerminalChip[];
  updatedAt: number;
}

export function emptyDraft(threadId = ''): Draft {
  return {
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  };
}
