// Which tab owns what the user types into the browser's address field: the
// one rule the address bar's draft follows (BrowserAddressBar.svelte).

/**
 * A tab switch the user chose from the address suggestions. It is an
 * intent until its selection is observed (the state shows that tab
 * active), superseded (the user made any later tab selection) or
 * impossible (the thread no longer has that tab), or it is answered
 * without any of them (the switch did not happen); the address bar drops
 * it then.
 */
export interface PendingSelection {
  readonly pageId: string;
  /** The state has shown this tab active since it was chosen. */
  readonly observed: boolean;
  /** The user made another tab selection since. */
  readonly superseded: boolean;
}

/**
 * The tab that owns what the user types: the tab a pending suggestion
 * switch is on its way to, since that gesture targeted it, until the
 * switch is observed or superseded or the thread has no such tab
 * (`hasTab`, the tabs as the state lists them now), so the owner is always
 * a tab that exists; otherwise the active tab, the one the user sees. Null
 * with neither.
 */
export function addressOwner(
  activePageId: string | null,
  pending: PendingSelection | null,
  hasTab: (pageId: string) => boolean,
): string | null {
  if (pending && !pending.observed && !pending.superseded && hasTab(pending.pageId)) return pending.pageId;
  return activePageId;
}
