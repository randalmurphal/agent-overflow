// The last setup file of the `unit` project: records what the test file
// starts from, once setup.ts has run (sharedWorkerRunner.ts).
import { SHARED_WORKER_KEY } from './config/sharedWorkerKeys';

const lifecycle = (globalThis as { [key: symbol]: { startFile(): void } | undefined })[Symbol.for(SHARED_WORKER_KEY)];
if (lifecycle === undefined) throw new Error("sharedWorkerStart.ts runs only under sharedWorkerRunner.ts, the unit project's runner");
lifecycle.startFile();
