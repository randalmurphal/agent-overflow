// The name a per-backend map is keyed by, and the name of the page's own
// backend. A leaf on purpose: every module that grew a per-backend map in
// phase 7 needs this constant, and several of them are imported BY
// ./backends.ts, so the constant cannot live there without a cycle.
//
// The key is the REGISTRY id (./backends.ts), not the backend's UUID. The
// UUID arrives with a manifest and is unknown at module load, which is
// exactly why it cannot key a map that has to exist before the first fetch
// resolves.

/**
 * The page's own backend.
 *
 * Empty string, and deliberately: it is what every per-backend API in this
 * app defaults its `backendId` parameter to, so a call site that names no
 * backend reads as "the one I have always meant" rather than as a sentinel
 * somebody has to look up. It is also what makes the single-backend app
 * behave identically — every existing call site keeps resolving here
 * without being touched.
 */
export const HOME_BACKEND = '';

/** A registry id. `HOME_BACKEND` for the page's own backend. */
export type BackendKey = string;
