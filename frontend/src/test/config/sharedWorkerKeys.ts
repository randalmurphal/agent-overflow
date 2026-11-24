// Worker globals of the `unit` project's file lifecycle, which its runner
// (sharedWorkerRunner.ts) installs once per worker.

/** What the import-tracking transform (importTrackerPlugin.ts) hands each
 * dynamic import to. */
export const TRACK_IMPORT_KEY = 'agent-overflow.test.trackImport';

/** The runner, as sharedWorkerStart.ts reaches it: a setup file is
 * evaluated afresh for each file and cannot import it. */
export const SHARED_WORKER_KEY = 'agent-overflow.test.sharedWorker';
