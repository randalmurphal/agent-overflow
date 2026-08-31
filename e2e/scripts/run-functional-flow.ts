// Standalone functional-flow driver. It owns both ends of the run: a fresh
// harness data directory and a fresh Playwright browser context. It never
// attaches to an existing page, CDP endpoint, or user profile.

import { lstat, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { chromium } from '@playwright/test';
import { launchHarness } from '../src/harness.ts';
import {
  FunctionalFlowRunner,
  ownPage,
  parseFunctionalScenario,
  type FunctionalFlowReport,
} from '../src/functional-flow.ts';
import type { CompatibilityLeg } from '../src/monitor-catalog.ts';
import { validateCompatibilityLeg } from '../src/monitor-catalog.ts';

interface Options {
  spec: string;
  report: string;
  headed: boolean;
  dataDir?: string;
  binary?: string;
  mockProvider?: string;
  compatibilityLeg?: string;
  pageID?: string;
  supervisedRoot?: boolean;
}

interface RunReport {
  v: 1;
  status: 'passed' | 'failed';
  spec: string;
  startedAt: string;
  finishedAt: string;
  ownership: {
    runId: string;
    dataRoot: string;
    dataDir: string;
    harnessPid: number;
    pageOrigin: string;
  } | null;
  result?: FunctionalFlowReport;
  error?: { message: string; stack?: string };
  lastObservations: Record<string, unknown>;
}

let activeCleanup: (() => Promise<void>) | undefined;
let signalExitStarted = false;
const CLEANUP_STAGE_TIMEOUT_MS = 10_000;

async function boundedCleanup<T>(label: string, operation: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<T>((_, reject) => {
        timer = setTimeout(() => reject(new Error(label + ' timed out after ' + CLEANUP_STAGE_TIMEOUT_MS + 'ms')), CLEANUP_STAGE_TIMEOUT_MS);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

function installSignalCleanup(): void {
  for (const [signal, code] of [['SIGINT', 130], ['SIGTERM', 143]] as const) {
    process.once(signal, () => {
      if (signalExitStarted) return;
      signalExitStarted = true;
      void (activeCleanup?.() ?? Promise.resolve()).finally(() => process.exit(code));
    });
  }
}

function usage(message?: string): never {
  if (message) process.stderr.write(`error: ${message}\n\n`);
  process.stderr.write(flowUsage());
  process.exit(2);
}

function flowUsage(): string {
  return 'usage: pnpm harness:flow --spec <scenario.json> [--report <report.json>] [--data-dir <empty-root>] [--binary <path>] [--mock-provider <path>] [--headed] [--leg <compatibility-leg>] [--page-id <page-id>]\n';
}

function parseArgs(argv: string[]): Options {
  let spec = '';
  let report = path.resolve(process.cwd(), 'functional-flow-report.json');
  let headed = false;
  let dataDir: string | undefined;
  let binary: string | undefined;
  let mockProvider: string | undefined;
  let compatibilityLeg: string | undefined;
  let pageID: string | undefined;
  let supervisedRoot = false;
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index];
    if (flag === '-h' || flag === '--help') {
      process.stdout.write(flowUsage());
      process.exit(0);
    }
    if (flag === '--supervised-root') { supervisedRoot = true; continue; }
    if (flag === '--headed') { headed = true; continue; }
    if (flag === '--leg') {
      const value = argv[++index];
      if (!value || value.startsWith('--')) usage('--leg needs a value');
      compatibilityLeg = validateCompatibilityLeg(value);
      continue;
    }
		if (flag === '--page-id') {
			const value = argv[++index];
			if (!value || value.startsWith('--')) usage('--page-id needs a value');
			pageID = value.trim();
			if (!pageID) usage('--page-id needs a non-empty value');
			continue;
		}
	    if (flag === '--data-dir' || flag === '--binary' || flag === '--mock-provider') {
	      const value = argv[++index];
	      if (!value || value.startsWith('--')) usage(`${flag} needs a value`);
	      if (flag === '--data-dir') dataDir = path.resolve(value);
	      else if (flag === '--binary') binary = path.resolve(value);
	      else mockProvider = path.resolve(value);
	      continue;
	    }
    if (flag === '--spec' || flag === '--report') {
      const value = argv[++index];
      if (!value || value.startsWith('--')) usage(`${flag} needs a value`);
      if (flag === '--spec') spec = path.resolve(value);
      else report = path.resolve(value);
      continue;
    }
    usage(`unknown argument ${JSON.stringify(flag)}`);
  }
  if (!spec) usage('--spec is required');
  return { spec, report, headed, dataDir, binary, mockProvider, compatibilityLeg, pageID, supervisedRoot };
}

async function main(options: Options): Promise<number> {
  const raw = JSON.parse(await readFile(options.spec, 'utf8')) as unknown;
  // Validate before launching anything. A malformed document must not leave a
  // backend or browser behind and must never be reported as a runtime failure.
  const scenario = parseFunctionalScenario(raw);
  if (options.dataDir) await requireEmptyDataDir(options.dataDir, options.supervisedRoot === true);
  const startedAt = new Date().toISOString();
  const runId = randomUUID();
  let browser: Awaited<ReturnType<typeof chromium.launch>> | undefined;
  let harness: Awaited<ReturnType<typeof launchHarness>> | undefined;
  const ownsDataDir = !options.dataDir;
  const flowDataDir = options.dataDir ?? await mkdtemp(path.join(tmpdir(), 'ao-functional-flow-'));
  let context: Awaited<ReturnType<NonNullable<typeof browser>['newContext']>> | undefined;
  let runner: FunctionalFlowRunner | undefined;
  let pageOrigin = '';
  let report: RunReport | undefined;
  let cleanupPromise: Promise<void> | undefined;
  const cleanup = (): Promise<void> => cleanupPromise ??= (async () => {
    if (context) {
      try {
        await boundedCleanup('close functional-flow browser context', context.close());
      } catch (error) {
        process.stderr.write('functional-flow cleanup: ' + (error as Error).message + '\n');
      }
    }
    let harnessClosed = !harness;
    if (harness) {
      try {
        await boundedCleanup('close functional-flow harness', harness.close());
        harnessClosed = true;
      } catch (error) {
        process.stderr.write('functional-flow cleanup: ' + (error as Error).message + '\n');
      }
    }
    if (browser) {
      try {
        await boundedCleanup('close functional-flow browser', browser.close());
      } catch (error) {
        process.stderr.write('functional-flow cleanup: ' + (error as Error).message + '\n');
      }
    }
    // A root is removed only after HarnessApp has proved that its backend
    // process tree is gone. If that proof timed out, leave the root for
    // postmortem inspection instead of racing a surviving writer.
    if (ownsDataDir && harnessClosed) {
      try {
        await boundedCleanup('remove functional-flow data dir ' + flowDataDir, rm(flowDataDir, { recursive: true, force: true }));
      } catch (error) {
        process.stderr.write('functional-flow cleanup: ' + (error as Error).message + '\n');
      }
    }
  })();
  activeCleanup = cleanup;
  try {
    browser = await chromium.launch({ headless: !options.headed });
    harness = await launchHarness({ dataDir: flowDataDir, binary: options.binary, mockProvider: options.mockProvider });
    context = await browser.newContext();
    const page = await context.newPage();
    await harness.open(page, { waitUntil: 'domcontentloaded' });
    const expectedOrigin = new URL(harness.url).origin;
    pageOrigin = new URL(page.url()).origin;
    if (pageOrigin !== expectedOrigin) throw new Error(`owned flow page navigated to ${pageOrigin}, expected ${expectedOrigin}`);
    let pageID = await waitForPageID(page);
		if (options.pageID && pageID !== options.pageID) {
			throw new Error(`owned flow page id ${pageID} does not match requested page id ${options.pageID}`);
		}
    let navigationGeneration = 0;
    let identityGeneration = navigationGeneration;
    page.on('framenavigated', (frame) => {
      if (frame === page.mainFrame()) navigationGeneration += 1;
    });
    runner = new FunctionalFlowRunner(ownPage(page), [], {
      runId,
      compatibilityLeg: options.compatibilityLeg as CompatibilityLeg | undefined,
      monitorQuery: async (spec) => {
        const currentOrigin = new URL(page.url()).origin;
        if (currentOrigin !== expectedOrigin) throw new Error(`owned flow page navigated to ${currentOrigin}, expected ${expectedOrigin}`);
        if (identityGeneration !== navigationGeneration) {
          pageID = await waitForPageID(page, pageID);
          identityGeneration = navigationGeneration;
        }
        const queryGeneration = navigationGeneration;
        try {
          return await harness!.rpc('HarnessUIQuery', { ...spec, pageId: pageID });
        } catch (error) {
          if (queryGeneration === navigationGeneration) throw error;
          pageID = await waitForPageID(page, pageID);
          identityGeneration = navigationGeneration;
          return harness!.rpc('HarnessUIQuery', { ...spec, pageId: pageID });
        }
      },
    });
    const result = await runner.run(scenario);
    report = {
      v: 1,
      status: 'passed',
      spec: options.spec,
      startedAt,
      finishedAt: new Date().toISOString(),
      ownership: { runId, dataRoot: harness.bootstrap.dataRoot, dataDir: harness.bootstrap.dataDir, harnessPid: harness.bootstrap.pid, pageOrigin },
      result,
      lastObservations: result.lastObservations,
    };
  } catch (error) {
    const failure = error instanceof Error ? error : new Error(String(error));
    report = {
      v: 1,
      status: 'failed',
      spec: options.spec,
      startedAt,
      finishedAt: new Date().toISOString(),
      ownership: harness ? { runId, dataRoot: harness.bootstrap.dataRoot, dataDir: harness.bootstrap.dataDir, harnessPid: harness.bootstrap.pid, pageOrigin } : null,
      error: { message: failure.message, ...(failure.stack ? { stack: failure.stack } : {}) },
      lastObservations: runner?.ui.lastObservations() ?? {},
    };
  } finally {
    await cleanup();
    activeCleanup = undefined;
  }
  await writeFile(options.report, `${JSON.stringify(report, null, 2)}\n`, 'utf8');
  process.stdout.write(`${report!.status}: ${scenario.id}\nreport: ${options.report}\n`);
  return report!.status === 'passed' ? 0 : 1;
}

async function requireEmptyDataDir(raw: string, supervisedRoot: boolean): Promise<void> {
  const resolved = path.resolve(raw);
  try {
    const info = await lstat(resolved);
    if (info.isSymbolicLink()) throw new Error(`--data-dir must not be a symlink: ${resolved}`);
    if (!info.isDirectory()) throw new Error(`--data-dir must be a directory: ${resolved}`);
    const entries = await readdir(resolved);
    const supervisorFiles = new Set(['run-manifest.json', 'run-lease.json', 'run-root-identity']);
    const unexpected = entries.filter((entry) => !supervisorFiles.has(entry));
    if (unexpected.length !== 0 || (!supervisedRoot && entries.length !== 0)) {
      throw new Error(`--data-dir must be an empty disposable directory: ${resolved}`);
    }
    if (supervisedRoot) {
      for (const entry of entries) {
        const child = await lstat(path.join(resolved, entry));
        if (!child.isFile()) throw new Error(`supervised --data-dir contains a non-file entry: ${path.join(resolved, entry)}`);
      }
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return;
    throw error;
  }
}

async function waitForPageID(page: import('@playwright/test').Page, previous?: string): Promise<string> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const pageID = new URL(page.url()).searchParams.get('pageId')?.trim() ?? '';
    if (pageID !== '' && pageID !== previous) return pageID;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error('harness page did not publish its pageId');
}

const options = parseArgs(process.argv.slice(2));
installSignalCleanup();
main(options).then((code) => { process.exitCode = code; }).catch(async (error: unknown) => {
  const failure = error instanceof Error ? error : new Error(String(error));
  const report = {
    v: 1 as const,
    status: 'failed' as const,
    spec: options.spec,
    startedAt: new Date().toISOString(),
    finishedAt: new Date().toISOString(),
    ownership: null,
    error: { message: failure.message, ...(failure.stack ? { stack: failure.stack } : {}) },
    lastObservations: {},
  };
  await writeFile(options.report, `${JSON.stringify(report, null, 2)}\n`, 'utf8');
  process.stderr.write(`functional flow failed before execution: ${failure.message}\nreport: ${options.report}\n`);
  process.exitCode = 1;
});
