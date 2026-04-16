export interface Thread {
  id: string;
  title: string;
  provider: 'claude' | 'codex';
  sessionRef?: string;
  workspacePath: string;
  projectPath: string;
  worktreePath?: string;
  branch?: string;
  interactionMode: 'default' | 'plan' | 'design' | 'discussion';
  discussionId?: string;
  parentThreadId?: string;
  model: string;
  createdAt: number;
  updatedAt: number;
  archived: boolean;
}

export interface Item {
  id: string;
  threadId: string;
  turnIndex: number;
  itemIndex: number;
  kind: string;
  role: string;
  summary: string;
  payloadId?: string;
  createdAt: number;
}

export interface PayloadMeta {
  id: string;
  threadId?: string;
  kind: string;
  meta: string; // JSON string — parse based on kind
  createdAt: number;
}

export interface DiffMeta {
  filePath: string;
  changeKind: 'added' | 'modified' | 'deleted' | 'renamed';
  insertions: number;
  deletions: number;
  preview: string;
}

export interface CommandOutputMeta {
  command: string;
  exitCode: number;
  lineCount: number;
  preview: string;
}

export interface ChangedFile {
  path: string;
  insertions: number;
  deletions: number;
  kind: string;
  payloadId: string;
}

export interface WorkEntryData {
  id: string;
  type: string;
  name?: string;
  status: 'running' | 'completed';
  meta?: unknown;
}
