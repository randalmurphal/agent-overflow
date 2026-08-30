import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { acquireProbeLease, activeProbeLeases } from './lease.mjs';

test('shared lease allows compatible observers and refuses conflicting instruments', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'ao-perfprobe-lease-'));
  const manifest = { leasePath: root };
  const releaseCounter = acquireProbeLease(manifest, 'realuse', 'counter');
  const releaseObserver = acquireProbeLease(manifest, 'jumpwatch', 'page-observer');
  assert.equal(activeProbeLeases().length, 2);
  assert.throws(
    () => acquireProbeLease(manifest, 'frames', 'trace'),
    /incompatible with active/,
  );
  releaseObserver();
  releaseCounter();
  assert.equal(readdirSync(root).length, 0);
  rmSync(root, { recursive: true, force: true });
});
