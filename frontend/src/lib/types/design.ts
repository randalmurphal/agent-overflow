// Design-mode types — mirror the Go structs in internal/design/types.go and
// internal/store/design_types.go. Update these if those files drift.

/**
 * Persisted metadata for an HTML artifact rendered in design mode.
 * HTML content is NOT included — fetch on demand via GetDesignArtifactHTML.
 */
export interface DesignArtifact {
  id: string;
  threadId: string;
  title: string;
  description: string;
  // `render` for plain render_design calls, `option` for present_options entries.
  kind: string;
  htmlPath: string;
  createdAt: number;
}

/**
 * One selectable option surfaced by a present_options tool call.
 * `artifactId` points at the stored DesignArtifact for this option's HTML.
 */
export interface DesignOption {
  id: string;
  title: string;
  description: string;
  artifactId: string;
}

/**
 * A blocked present_options request. The agent is waiting on the user to pick
 * one of `options`; the frontend resolves the request by calling
 * ChooseDesignOption(threadId, requestId, optionId).
 */
export interface DesignOptionsRequest {
  requestId: string;
  threadId: string;
  prompt: string;
  options: DesignOption[];
}

/**
 * Resolution notice emitted when a design option is chosen (or the session
 * is torn down). Matches the payload emitted by the Go reactor on
 * `design:chosen`.
 */
export interface DesignChoiceResolved {
  threadId: string;
  requestId: string;
  optionId: string;
  title: string;
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
