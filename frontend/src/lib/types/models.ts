// Re-export Wails-generated types for convenience.
// These are the canonical types matching Go structs exactly.
export { store } from '../../../wailsjs/go/models';
export type Thread = import('../../../wailsjs/go/models').store.Thread;
export type Item = import('../../../wailsjs/go/models').store.Item;
export type PayloadMeta = import('../../../wailsjs/go/models').store.PayloadMeta;

// Meta types parsed from PayloadMeta.meta JSON string.
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

export interface ThinkingMeta {
  tokenCount: number;
  preview: string;
}
