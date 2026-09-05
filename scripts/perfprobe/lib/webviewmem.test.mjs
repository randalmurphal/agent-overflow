import test from 'node:test';
import assert from 'node:assert/strict';
import { samplerIdentity } from './webviewmem.mjs';

const manifest = { instanceId: 'owned', origin: 'http://127.0.0.1:1234', target: { targetId: 'page', pageMarker: 'marker' } };

test('passes the documented nested target as a complete PowerShell identity', () => {
  assert.deepEqual(samplerIdentity(manifest, 123), {
    instanceId: 'owned', origin: manifest.origin, targetId: 'page', pageMarker: 'marker', browserPid: 123,
  });
  assert.equal(samplerIdentity({ ...manifest, browserPid: 123 }, 123).browserPid, 123);
  assert.throws(() => samplerIdentity({ ...manifest, browserPid: 456 }, 123), /differs/);
  assert.throws(() => samplerIdentity({ ...manifest, target: {} }, 123), /targetId/);
  assert.throws(() => samplerIdentity(manifest, 0), /positive browser PID/);
});
