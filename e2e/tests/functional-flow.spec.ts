import { test, expect } from './fixtures.js';
import {
  FunctionalFlowRunner,
  defineFlowExtension,
  ownPage,
  parseFunctionalScenario,
  type SemanticTarget,
} from '../src/functional-flow.js';

const input: SemanticTarget = { label: 'Name' };

test('runs real semantic input and reports native control state', async ({ page }) => {
  await page.setContent(`
    <label>Name <input aria-label="Name" /></label>
    <button aria-label="Continue">Continue</button>
    <input aria-label="Disabled" disabled />`);
  const runner = new FunctionalFlowRunner(ownPage(page));
  const report = await runner.run({
    v: 1,
    id: 'form-input',
    actions: [
      { kind: 'fill', target: input, value: 'Ada' },
      { kind: 'focus', target: input },
      { kind: 'key', target: input, key: 'End' },
      { kind: 'type', target: input, text: ' Lovelace' },
      { kind: 'click', target: { role: 'button', name: 'Continue' } },
    ],
    assertions: [
      { kind: 'value', target: input, expected: 'Ada Lovelace' },
      { kind: 'focused', target: { role: 'button', name: 'Continue' }, expected: true },
      { kind: 'disabled', target: { label: 'Disabled' }, expected: true },
      { kind: 'attribute', target: { role: 'button', name: 'Continue' }, name: 'aria-label', expected: 'Continue' },
    ],
    monitors: [{ id: 'name', target: input, durationMs: 100, intervalMs: 25 }],
  });
  expect(report.v).toBe(1);
  expect(report.observations.filter((sample) => sample.monitorId === 'name').length).toBeGreaterThan(1);
  expect(report.lastObservations[JSON.stringify(input)]?.value).toBe('Ada Lovelace');
});

test('lets monitors finish their declared window after actions complete', async ({ page }) => {
  await page.setContent('<p data-testid="status">ready</p>');
  const runner = new FunctionalFlowRunner(ownPage(page));
  const report = await runner.run({
    v: 1,
    id: 'monitor-window',
    actions: [],
    monitors: [{ id: 'status', target: { testId: 'status' }, durationMs: 100, intervalMs: 10 }],
  });
  expect(report.monitors).toMatchObject([{ monitorId: 'status', status: 'complete' }]);
  expect(report.monitors[0]?.heartbeats).toBeGreaterThan(1);
});

test('rejects version drift, unknown fields, and ambiguous action targets', async ({ page }) => {
  expect(() => parseFunctionalScenario({ v: 2, id: 'future', actions: [] })).toThrow('scenario.v');
  expect(() => parseFunctionalScenario({ v: 1, id: 'typo', actions: [], nope: true })).toThrow('scenario.nope');
  await page.setContent('<button>One</button><button>Two</button>');
  const runner = new FunctionalFlowRunner(ownPage(page));
  await expect(runner.run({ v: 1, id: 'ambiguous', actions: [{ kind: 'click', target: { role: 'button' } }] })).rejects.toThrow('exactly one');
});

test('accepts exact matching for every locator strategy that supports it', () => {
  for (const target of [
    { role: 'button', name: 'Save', exact: true },
    { label: 'Name', exact: true },
    { text: 'Save', exact: true },
    { placeholder: 'Name', exact: true },
  ]) {
    expect(() => parseFunctionalScenario({
      v: 1,
      id: 'exact-target',
      actions: [{ kind: 'click', target }],
    })).not.toThrow();
  }
});

test('requires explicit typed monitor identity and compatibility leg', async () => {
  expect(() => parseFunctionalScenario({
    v: 1,
    id: 'implicit-monitor',
    actions: [],
    monitors: [{ id: 'frame-pacing', target: { testId: 'x' }, durationMs: 10 }],
  })).not.toThrow();
  const semantic = parseFunctionalScenario({
    v: 1,
    id: 'semantic-monitor',
    actions: [],
    monitors: [{ id: 'frame-pacing', target: { testId: 'x' }, durationMs: 10 }],
  });
  expect(semantic.monitors?.[0]?.monitorId).toBeUndefined();
  expect(() => parseFunctionalScenario({
    v: 1,
    id: 'typed-monitor',
    actions: [],
    monitors: [{ id: 'frame-pacing', monitorId: 'frame-pacing', durationMs: 10 }],
  })).toThrow('compatibilityLeg is required');
  expect(() => parseFunctionalScenario({
    v: 1,
    id: 'bad-leg',
    actions: [],
    monitors: [{ id: 'custom', target: { testId: 'x' }, compatibilityLeg: 'bogus', durationMs: 10 }],
  })).toThrow('compatibilityLeg requires an explicit monitorId');
});

test('hidden and absent semantic targets settle without a timeout', async ({ page }) => {
  await page.setContent('<p data-testid="shown">ready</p>');
  const runner = new FunctionalFlowRunner(ownPage(page));
  await expect(runner.run({
    v: 1,
    id: 'hidden-missing',
    actions: [],
    assertions: [{ kind: 'hidden', target: { testId: 'missing' } }],
  })).resolves.toMatchObject({ scenario: 'hidden-missing' });
});

test('extensions cannot reach Playwright internals or inherited operations', async ({ page }) => {
  await page.setContent('<p data-testid="status">ready</p>');
  let privateFields: unknown[] = [];
  const extension = defineFlowExtension({
    name: 'safe',
    actions: {
      inspect: async ({ ui }) => {
        privateFields = [
          (ui as unknown as { page?: unknown }).page,
          (ui.locator({ testId: 'status' }) as unknown as { raw?: unknown }).raw,
        ];
      },
    },
  });
  const runner = new FunctionalFlowRunner(ownPage(page), [extension]);
  await runner.run({ v: 1, id: 'safe-extension', actions: [], extensions: [{ extension: 'safe', operation: 'inspect' }] });
  expect(privateFields).toEqual([undefined, undefined]);
  await expect(runner.run({ v: 1, id: 'inherited-operation', actions: [], extensions: [{ extension: 'safe', operation: 'toString' }] })).rejects.toThrow('has no operation');
});

test('rejects a flow after its owned page crosses origins and excludes concurrent owners', async ({ page }) => {
  await page.setContent('<p data-testid="status">ready</p>');
  const owned = ownPage(page);
  expect(ownPage(page)).toBe(owned);
  const first = new FunctionalFlowRunner(owned);
  const second = new FunctionalFlowRunner(owned);
  const active = first.run({ v: 1, id: 'active', actions: [], monitors: [{ id: 'status', target: { testId: 'status' }, durationMs: 100, intervalMs: 10 }] });
  await expect(second.run({ v: 1, id: 'concurrent', actions: [] })).rejects.toThrow('active run');
  await active;
  await page.goto('https://example.com', { waitUntil: 'commit' });
  expect(() => owned.assertCurrent()).toThrow('owned flow page navigated');
});

test('cancels and joins monitors when an action fails', async ({ page }) => {
  await page.setContent('<p data-testid="status">ready</p>');
  const runner = new FunctionalFlowRunner(ownPage(page));
  await expect(runner.run({
    v: 1,
    id: 'cancel-monitor',
    actions: [{ kind: 'click', target: { testId: 'missing' } }],
    monitors: [{ id: 'status', target: { testId: 'status' }, durationMs: 1_000, intervalMs: 10 }],
  })).rejects.toThrow('exactly one');
  const last = runner.ui.lastObservations();
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(runner.ui.lastObservations()).toEqual(last);
});

test('stops a typed monitor after an injected heartbeat fault', async ({ page }) => {
  await page.setContent('<p>ready</p>');
  const operations: string[] = [];
  const runner = new FunctionalFlowRunner(ownPage(page), [], {
    monitorQuery: async (spec) => {
      operations.push(String(spec.op));
      if (spec.op === 'heartbeat') throw new Error('injected monitor fault');
      if (spec.op === 'start') return { v: 1, runId: spec.runId, startedAtMs: 1, monitors: [], overlap: [] };
      return { v: 1, runId: spec.runId, status: 'partial', heartbeats: 0, monitors: [], overlap: [], errors: ['injected monitor fault'] };
    },
  });
  await expect(runner.run({
    v: 1,
    id: 'fault-monitor',
    actions: [],
    monitors: [{ id: 'semantic-dom-stability', monitorId: 'semantic-dom-stability', compatibilityLeg: 'functional', durationMs: 100, intervalMs: 1 }],
  })).rejects.toThrow('injected monitor fault');
  expect(operations).toEqual(['start', 'heartbeat', 'stop']);
});

test('extensions receive semantic UI only', async ({ page }) => {
  await page.setContent('<p data-testid="status">ready</p>');
  const extension = defineFlowExtension<{ expected: string }>({
    name: 'checks',
    assertions: {
      textIs: async ({ ui }, inputValue) => {
        await ui.assert({ testId: 'status' }, { kind: 'text', target: { testId: 'status' }, expected: inputValue.expected }, 1_000);
      },
    },
  });
  const runner = new FunctionalFlowRunner(ownPage(page), [extension]);
  await expect(runner.run({
    v: 1,
    id: 'extension',
    actions: [],
    extensions: [{ extension: 'checks', operation: 'textIs', input: { expected: 'ready' } }],
})).resolves.toMatchObject({ scenario: 'extension' });
});

test('drives pointer, wheel and viewport input without page evaluation', async ({ page }) => {
  await page.setContent(`
    <button data-testid="approve">Approve</button>
    <div data-testid="source" draggable="true" style="width:20px;height:20px">source</div>
    <div data-testid="drop" style="width:100px;height:30px">drop</div>
    <div data-testid="scroll" style="width:100px;height:40px;overflow:auto"><div style="height:500px">content</div></div>`);
  const runner = new FunctionalFlowRunner(ownPage(page));
  await expect(runner.run({
    v: 1,
    id: 'pointer-input',
    actions: [
      { kind: 'approve', target: { testId: 'approve' } },
      { kind: 'drag', source: { testId: 'source' }, target: { testId: 'drop' } },
      { kind: 'wheel', target: { testId: 'scroll' }, deltaY: 80 },
      { kind: 'viewport', width: 800, height: 600 },
    ],
  })).resolves.toMatchObject({ scenario: 'pointer-input' });
});

test('selects a registered monitor and persists owned heartbeat evidence', async ({ harness, page }) => {
  await harness.open(page);
  const deadline = Date.now() + 10_000;
  let pageID = '';
  while (Date.now() < deadline && pageID === '') {
    pageID = new URL(page.url()).searchParams.get('pageId')?.trim() ?? '';
    if (pageID === '') await new Promise((resolve) => setTimeout(resolve, 25));
  }
  expect(pageID).not.toBe('');
  const runner = new FunctionalFlowRunner(ownPage(page), [], {
    runId: 'flow-owned',
    compatibilityLeg: 'functional',
    monitorQuery: (spec) => harness.rpc('HarnessUIQuery', { ...spec, pageId: pageID }),
  });
  const report = await runner.run({
    v: 1,
    id: 'registered-monitor',
    actions: [],
    monitors: [{ id: 'semantic-dom-stability', monitorId: 'semantic-dom-stability', compatibilityLeg: 'functional', durationMs: 30, intervalMs: 10 }],
  });
  expect(report.runId).toBe('flow-owned');
  expect(report.monitors).toMatchObject([{ monitorId: 'semantic-dom-stability', runId: 'flow-owned/semantic-dom-stability', status: 'complete' }]);
  expect(report.monitors[0]?.typed).toMatchObject({ runId: 'flow-owned/semantic-dom-stability', status: 'complete' });
  expect(() => parseFunctionalScenario({ v: 1, id: 'wrong-leg', actions: [], monitors: [{ id: 'frame-pacing', monitorId: 'frame-pacing', compatibilityLeg: 'functional', durationMs: 10 }] })).toThrow('belongs to clean-renderer');
});
