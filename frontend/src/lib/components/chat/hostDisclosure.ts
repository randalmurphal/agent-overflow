/**
 * A disclosure a HOST owns for a header-only row (AgentRow, CollabToolRow).
 * The background tray expands an agent row into its digest; the row keeps
 * rendering the header and the host decides what the chevron opens. A row
 * given one of these renders its chevron from it and routes the header
 * click to `onToggle`; `expandable: false` grays the chevron.
 */
export interface HostDisclosure {
  expandable: boolean;
  expanded: boolean;
  /** DOM id of the body the header controls, when expandable. */
  controls?: string;
  onToggle(): void;
}
