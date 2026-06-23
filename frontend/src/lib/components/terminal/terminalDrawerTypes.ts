export interface ThreadTerminalSurfaceContext {
  paneId: string;
  threadId: string | null;
  workspacePath: string | undefined;
  setVisible(value: boolean): void;
  acquireResizeLease(): (() => void) | null;
  canAdoptOpenedTerminal?(threadID: string, workspacePath: string | undefined): boolean;
  /**
   * Read-and-clear the pane's "focus the terminal on open" intent. The drawer
   * calls this once when it mounts; a true result means latch focus into the
   * xterm as soon as TerminalBody binds.
   */
  consumeFocusRequest(): boolean;
  /**
   * Re-settle the scroll controller after the drawer mounts ASYNCHRONOUSLY.
   * On a cold first open the drawer is a lazy `import()`, so the real in-flow
   * `shrink-0` drawer box (120–320px) commits a few frames after the open
   * gesture's 2-rAF settle lease has already released — and the controller has
   * no `scrollEl` ResizeObserver to catch the resulting `clientHeight` drop.
   * The chat-mode placement implements this as a `leaseDuringSettle` cycle so
   * a stuck-to-bottom timeline re-pins instead of leaving the latest messages
   * hidden behind the drawer. Optional: surfaces with no scroll controller
   * (full-pane TerminalView) omit it.
   */
  settleAfterAsyncMount?(): void;
}

export interface ThreadTerminalDrawerProps {
  surface: ThreadTerminalSurfaceContext;
  /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
  manual?: boolean;
}
