import type { CompatibilityLeg } from './monitor-catalog.ts';
import {
  FUNCTIONAL_FLOW_VERSION,
  type FlowAction,
  type FlowExtensionContext,
  type FlowExtensionInvocation,
  type FlowMonitorResult,
  type FlowMonitorSample,
  type FunctionalFlowExtension,
  type FunctionalFlowReport,
  type FunctionalScenario,
} from './flow-model.ts';
import { parseFunctionalScenario } from './flow-validation.ts';
import { isOwnedPage, OwnedPage } from './flow-page.ts';
import { SemanticUI } from './flow-ui.ts';
import { runFlowMonitor } from './flow-monitors.ts';

export function defineFlowExtension<I>(extension: FunctionalFlowExtension<I>): FunctionalFlowExtension<I> { return extension; }

export class FunctionalFlowRunner {
  readonly ui: SemanticUI;
  private readonly extensions: Map<string, FunctionalFlowExtension<any>>;
  private running = false;
  constructor(private readonly page: OwnedPage, extensions: FunctionalFlowExtension<any>[] = [], private readonly runOptions: {
    runId?: string;
    compatibilityLeg?: CompatibilityLeg;
    monitorQuery?: (spec: Record<string, unknown>) => Promise<unknown>;
  } = {}) {
    if (!isOwnedPage(page)) throw new Error('FunctionalFlowRunner requires a page created with ownPage()');
    this.ui = new SemanticUI(page); this.extensions = new Map(extensions.map((extension) => [extension.name, extension]));
    if (this.extensions.size !== extensions.length) throw new Error('flow extension names must be unique');
  }
  async run(rawScenario: FunctionalScenario | unknown): Promise<FunctionalFlowReport> {
    if (this.running) throw new Error('functional flow runner already has an active run');
    this.running = true;
    let releasePage: (() => void) | undefined;
    try {
      const scenario = parseFunctionalScenario(rawScenario);
      releasePage = this.page.claimRun();
      const observations: FlowMonitorSample[] = [];
      const monitorResults: FlowMonitorResult[] = [];
      const runId = this.runOptions.runId ?? `${scenario.id}-${Date.now()}`;
      const cancellation = new AbortController();
      // A successful action sequence does not end the monitoring window. Each
      // monitor owns its declared duration, so a short action can still be
      // paired with a longer app-feel observation. An action failure is the
      // opposite case: stop monitors promptly, then join them so partial
      // evidence is recorded before the flow rejects.
      const actionPromise = this.runActions(scenario.actions, cancellation.signal).catch((error: unknown) => {
        cancellation.abort();
        throw error;
      });
      const monitorHost = { ui: this.ui, compatibilityLeg: this.runOptions.compatibilityLeg, monitorQuery: this.runOptions.monitorQuery };
      const monitors = (scenario.monitors ?? []).map((monitor) => runFlowMonitor(monitorHost, monitor, observations, monitorResults, runId, cancellation.signal)
        .catch((error: unknown) => {
          cancellation.abort();
          throw error;
        }));
      try {
        await Promise.all([actionPromise, ...monitors]);
      } finally {
        cancellation.abort();
        await Promise.allSettled([actionPromise, ...monitors]);
      }
      for (const assertion of scenario.assertions ?? []) await this.ui.assert(assertion.target, assertion);
      for (const extension of scenario.extensions ?? []) await this.runExtension(extension);
      const overlaps = monitorResults.flatMap((monitor, index) => monitorResults.slice(index + 1)
        .filter((other) => monitor.startedAtMs < other.stoppedAtMs && other.startedAtMs < monitor.stoppedAtMs)
        .map((other) => ({ runId: monitor.runId, withRunId: other.runId, atMs: Math.max(monitor.startedAtMs, other.startedAtMs) })));
      return { v: FUNCTIONAL_FLOW_VERSION, runId, scenario: scenario.id, observations, monitors: monitorResults, overlaps, lastObservations: this.ui.lastObservations() };
    } finally {
      releasePage?.();
      this.running = false;
    }
  }
  private async runActions(actions: FlowAction[], signal: AbortSignal): Promise<void> {
    for (const action of actions) {
      if (signal.aborted) throw new Error('functional flow cancelled');
      if (action.kind === 'viewport') { await this.page.viewport(action.width, action.height); continue; }
      if (action.kind === 'drag') { const source = this.ui.locator(action.source); const target = this.ui.locator(action.target); await source.requireActionable('drag'); await target.requireActionable('drag'); await source.dragTo(target); continue; }
      const locator = this.ui.locator(action.target); await locator.requireActionable(action.kind);
      if (action.kind === 'click' || action.kind === 'approve') await locator.click();
      else if (action.kind === 'focus') await locator.focus();
      else if (action.kind === 'fill') await locator.fill(action.value);
      else if (action.kind === 'type') await locator.type(action.text, action.delayMs);
      else if (action.kind === 'key') await locator.key(action.key);
      else if (action.kind === 'wheel') await this.page.wheel(action.target, action.deltaX ?? 0, action.deltaY);
    }
  }
  private async runExtension(invocation: FlowExtensionInvocation): Promise<void> {
    const extension = this.extensions.get(invocation.extension); if (!extension) throw new Error(`unknown flow extension ${JSON.stringify(invocation.extension)}`);
    const operation = ownOperation(extension.actions, invocation.operation) ?? ownOperation(extension.assertions, invocation.operation);
    if (!operation) throw new Error(`extension ${JSON.stringify(invocation.extension)} has no operation ${JSON.stringify(invocation.operation)}`);
    const context: FlowExtensionContext = { ui: this.ui, observe: (target) => this.ui.observe(target) };
    await operation(context, invocation.input);
  }
}

function ownOperation(
  operations: Record<string, ((context: FlowExtensionContext, input: unknown) => Promise<void>) | undefined> | undefined,
  name: string,
): ((context: FlowExtensionContext, input: unknown) => Promise<void>) | undefined {
  if (!operations || !Object.prototype.hasOwnProperty.call(operations, name)) return undefined;
  const operation = operations[name];
  if (typeof operation !== 'function') throw new Error(`flow extension operation ${JSON.stringify(name)} is not callable`);
  return operation;
}
