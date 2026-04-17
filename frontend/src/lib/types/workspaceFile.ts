export interface WorkspaceFile {
  path: string;
  kind: 'file' | 'directory';
  parentPath?: string;
}

export interface WorkspaceFileSearchResult {
  files: WorkspaceFile[];
  truncated: boolean;
  root: string;
}
