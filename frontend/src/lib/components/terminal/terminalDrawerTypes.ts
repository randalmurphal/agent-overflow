import type { ThreadPane } from '../../stores/thread.svelte';

export interface SendToComposerChip {
  id: string;
  label: string;
  preview: string;
  content: string;
  createdAt: number;
}

export interface ThreadTerminalDrawerProps {
  pane: ThreadPane;
  /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
  manual?: boolean;
  /** Called when the user captures selected terminal text as a chip. */
  onSendToComposer?: (chip: SendToComposerChip) => void;
}
