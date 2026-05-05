// Design-mode types — mirror the Go structs in internal/design/types.go and
// internal/store/design_types.go. Update these if those files drift.

/**
 * Persisted metadata for a frozen state of a design thread's working
 * directory. Snapshots are created on explicit user gesture and
 * auto-on-turn-start; a snapshot's dirPath points at a directory holding
 * the working files at that moment.
 */
export interface DesignSnapshot {
  id: string;
  threadId: string;
  label: string;
  dirPath: string;
  parentSnapshotId?: string;
  auto: boolean;
  createdAt: number;
}

/** Severity classifier for a captured runtime event. */
export type DiagnosticSeverity = 'error' | 'warn' | 'info';

/**
 * One captured runtime event from the sandboxed iframe. Tokens are
 * monotonic per thread; agents pass `since_token` to drain only what
 * they haven't seen.
 */
export interface Diagnostic {
  token: number;
  severity: DiagnosticSeverity;
  message: string;
  source?: string;
  line?: number;
  column?: number;
  stack?: string;
  url?: string;
  createdAt: number;
}

/**
 * Wire payload from the iframe-injected capture script forwarded by the
 * frontend over WebSocket.
 */
export interface DiagnosticBatch {
  threadId: string;
  diagnostics: Diagnostic[];
}

/** One slider knob update inside a feedback batch. */
export interface SliderChange {
  id: string;
  value: number;
}

/**
 * Accumulated user feedback for one round trip. The frontend serialises
 * this and sends it as a regular user message; the agent reads it as
 * input on the next turn.
 */
export interface FeedbackBatch {
  sliderChanges?: SliderChange[];
  notes?: string;
}

/** One selectable answer within a clarification question. */
export interface ClarificationChoice {
  id: string;
  label: string;
}

/** A single multiple-choice clarification question. */
export interface ClarificationQuestion {
  id: string;
  prompt: string;
  choices: ClarificationChoice[];
  multiple?: boolean;
}

/**
 * Emitted by the agent as a structured assistant-text payload when it
 * needs the user to commit to a design direction before continuing.
 */
export interface ClarificationRequest {
  requestId: string;
  threadId: string;
  intro?: string;
  questions: ClarificationQuestion[];
}

/**
 * One agent-emitted slider exposed in the feedback panel after a design
 * iteration lands.
 */
export interface SliderControl {
  id: string;
  label: string;
  min: number;
  max: number;
  step?: number;
  value: number;
}

/**
 * Active option-set state — when non-null, the design pane shows the
 * options grid. `optionPaths` is the list of /design/{threadId}/options/{setId}/{optionId}
 * directories the agent placed in the working tree.
 */
export interface ActiveOptionSet {
  setId: string;
  optionPaths: string[];
}

export type DesignViewport = 'mobile' | 'tablet' | 'desktop';

/**
 * Pixel widths used by the viewport toggle. `desktop` fills the container.
 */
export const DESIGN_VIEWPORT_WIDTHS: Record<DesignViewport, number | null> = {
  mobile: 375,
  tablet: 768,
  desktop: null,
};
