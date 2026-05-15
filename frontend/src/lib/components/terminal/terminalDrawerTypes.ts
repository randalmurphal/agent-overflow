export interface SendToComposerChip {
  id: string;
  label: string;
  preview: string;
  content: string;
  createdAt: number;
}

export interface ThreadTerminalSurfaceContext {
  paneId: string;
  threadId: string | null;
  workspacePath: string | undefined;
  setVisible(value: boolean): void;
  acquireResizeLease(): (() => void) | null;
  sendTerminalChip(chip: SendToComposerChip): void;
}

export interface ThreadTerminalDrawerProps {
  surface: ThreadTerminalSurfaceContext;
  /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
  manual?: boolean;
}
