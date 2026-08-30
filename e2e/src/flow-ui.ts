import { expect } from '@playwright/test';
import type { FlowAssertion, SemanticObservation, SemanticTarget } from './flow-model.ts';
import { MAX_FLOW_WAIT_MS } from './flow-model.ts';
import { OwnedPage, type SemanticLocator } from './flow-page.ts';

export class SemanticUI {
  private readonly last = new Map<string, SemanticObservation>();
  readonly #page: OwnedPage;
  constructor(page: OwnedPage) { this.#page = page; }
  locator(target: SemanticTarget): SemanticLocator { return this.#page.locator(target); }
  async observe(target: SemanticTarget, attributeNames: string[] = []): Promise<SemanticObservation> { const key = JSON.stringify(target); const observation = await this.locator(target).observe(attributeNames); this.last.set(key, observation); return observation; }
  lastObservations(): Record<string, SemanticObservation> { return Object.fromEntries(this.last); }
  async await(assertion: FlowAssertion, timeoutMs?: number): Promise<SemanticObservation>;
  async await(target: SemanticTarget, assertion: FlowAssertion, timeoutMs?: number): Promise<SemanticObservation>;
  async await(targetOrAssertion: SemanticTarget | FlowAssertion, assertionOrTimeout?: FlowAssertion | number, timeoutMs = 10_000): Promise<SemanticObservation> {
    const assertion = typeof assertionOrTimeout === 'object' ? assertionOrTimeout : targetOrAssertion as FlowAssertion;
    const target = typeof assertionOrTimeout === 'object' ? targetOrAssertion as SemanticTarget : assertion.target;
    if (typeof assertionOrTimeout === 'number') timeoutMs = assertionOrTimeout;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0 || timeoutMs > MAX_FLOW_WAIT_MS) {
      throw new Error(`flow wait must be between 1 and ${MAX_FLOW_WAIT_MS}ms`);
    }
    let last: SemanticObservation | undefined;
    try {
      await expect.poll(async () => { last = await this.observe(target, assertion.kind === 'attribute' ? [assertion.name] : []); return matches(last, assertion); }, { timeout: timeoutMs, intervals: [50, 100, 250] }).toBe(true);
    } catch (error) {
      throw new Error(`${formatFailure(assertion, last)}: ${String(error)}`, { cause: error });
    }
    return last!;
  }
  assert(assertion: FlowAssertion, timeoutMs?: number): Promise<SemanticObservation>;
  assert(target: SemanticTarget, assertion: FlowAssertion, timeoutMs?: number): Promise<SemanticObservation>;
  assert(targetOrAssertion: SemanticTarget | FlowAssertion, assertionOrTimeout?: FlowAssertion | number, timeoutMs?: number): Promise<SemanticObservation> {
    return typeof assertionOrTimeout === 'object'
      ? this.await(targetOrAssertion as SemanticTarget, assertionOrTimeout, timeoutMs)
      : this.await(targetOrAssertion as FlowAssertion, assertionOrTimeout);
  }
}

function matches(observation: SemanticObservation, assertion: FlowAssertion): boolean {
  if (assertion.kind === 'count') return observation.count === assertion.expected;
  if (assertion.kind === 'visible') return observation.count === 0 ? assertion.expected === false : observation.count === 1 && observation.visible === assertion.expected;
  if (assertion.kind === 'hidden') return observation.count === 0 ? assertion.expected === true : observation.count === 1 && observation.visible === !assertion.expected;
  if (observation.count !== 1) return false;
  if (assertion.kind === 'text') return observation.text === assertion.expected;
  if (assertion.kind === 'value') return observation.value === assertion.expected;
  if (assertion.kind === 'attribute') return observation.attributes[assertion.name] === assertion.expected;
  return observation[assertion.kind] === assertion.expected;
}

function formatFailure(assertion: FlowAssertion, observation: SemanticObservation | undefined): string { return `semantic assertion ${assertion.kind} failed; last observation: ${JSON.stringify(observation)}`; }
