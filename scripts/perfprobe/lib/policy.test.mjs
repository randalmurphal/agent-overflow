import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROBE_POLICIES,
  instrumentsCompatible,
  methodKind,
  methodAllowed,
  policyForProbe,
} from './policy.mjs';

test('every online probe has one declarative policy', async () => {
  const { readdirSync, readFileSync } = await import('node:fs');
  for (const name of readdirSync(new URL('..', import.meta.url))) {
    if (!name.endsWith('.mjs') || name === 'realuse-report.mjs') continue;
    const source = readFileSync(new URL(`../${name}`, import.meta.url), 'utf8');
    if (!source.includes('lib/cdp.mjs')) continue;
    assert.doesNotThrow(() => policyForProbe(name.slice(0, -4)), name);
  }
});

test('instrument classes refuse incompatible sessions', () => {
  assert.equal(instrumentsCompatible('counter', 'page-observer'), true);
  assert.equal(instrumentsCompatible('trace', 'trace'), true);
  assert.equal(instrumentsCompatible('trace', 'profiler'), false);
  assert.equal(instrumentsCompatible('mutate', 'page-observer'), false);
  assert.equal(methodKind('Input.synthesizeScrollGesture'), 'mutate');
  assert.equal(methodKind('Memory.simulatePressureNotification'), 'mutate');
  assert.equal(methodKind('Tracing.start'), 'trace');
  assert.equal(methodKind('HeapProfiler.startSampling'), 'profiler');
  assert.equal(methodAllowed(policyForProbe('realuse'), 'Runtime.evaluate'), true);
  assert.equal(methodAllowed(policyForProbe('realuse'), 'Tracing.start'), false);
});

test('screen capture probes are absent from the online surface', () => {
  for (const name of ['screenshot', 'spritecheck3', 'spritecheck4', 'jumpbottom']) {
    assert.equal(PROBE_POLICIES[name], undefined, `${name} must not have an online policy`);
    assert.throws(() => policyForProbe(name), /no declarative online probe policy/);
  }
});
