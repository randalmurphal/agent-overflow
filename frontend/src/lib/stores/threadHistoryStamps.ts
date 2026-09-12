/** A history revision paired with the window returned by SyncThreadWindow. */
export interface ThreadHistoryStamp {
  epoch: number;
  rev: number;
  attested: boolean;
}

/** Forces a page when the client has no attested window to validate. */
export const UNKNOWN_STAMP_VALUE = -1;
