export interface ThreadTerminalSurfaceContext {
  paneId: string;
  threadId: string | null;
  workspacePath: string | undefined;
  setVisible(value: boolean): void;
  acquireResizeLease(): (() => void) | null;
}

export interface ThreadTerminalDrawerProps {
  surface: ThreadTerminalSurfaceContext;
  /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
  manual?: boolean;
}
