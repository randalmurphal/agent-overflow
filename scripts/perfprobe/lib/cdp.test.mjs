import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

test('port normalization rejects ambiguous endpoints', async () => {
  const { normalizePort } = await import('./cdp.mjs?port-test');
  assert.equal(normalizePort('009226'), '9226');
  assert.throws(() => normalizePort('9226:1'), /must be an integer/);
  assert.throws(() => normalizePort('65536'), /between 1 and 65535/);
});

test('manifest validation requires the exact page identity without a fallback', async () => {
  const root = mkdtempSync(path.join(tmpdir(), 'ao-perfprobe-manifest-'));
  const manifestPath = path.join(root, 'instance.json');
  writeFileSync(manifestPath, JSON.stringify({
    instanceId: 'perf-instance',
    origin: 'http://127.0.0.1:9226',
    target: { id: 'page-1', marker: 'ao-instance-perf' },
    leasePath: path.join(root, 'leases'),
  }));
  process.env.AO_PERFPROBE_MANIFEST = manifestPath;
  const { loadInstanceManifest, validateManifestTarget } = await import('./cdp.mjs?manifest-test');
  const manifest = loadInstanceManifest();
  const target = {
    type: 'page',
    id: 'page-1',
    title: 'Agent Overflow ao-instance-perf',
    url: 'http://127.0.0.1:9226/?page=ao-instance-perf',
    webSocketDebuggerUrl: 'ws://127.0.0.1:9226/devtools/page/page-1',
  };
  assert.equal(validateManifestTarget(target, manifest), target);
  assert.throws(() => validateManifestTarget({ ...target, id: 'page-2' }, manifest), /not the supervisor target/);
  assert.throws(() => validateManifestTarget({ ...target, title: 'Agent Overflow', url: 'http://127.0.0.1:9226/' }, manifest), /page marker/);
  assert.throws(() => validateManifestTarget({ ...target, title: 'Agent Overflow ao-instance-perf', url: 'http://127.0.0.1:9226/' }, manifest), /page marker/);
  rmSync(root, { recursive: true, force: true });
});

test('streamed CDP results stop at the configured byte bound', async () => {
  const previous = process.env.AO_PROBE_MAX_STREAM_BYTES;
  process.env.AO_PROBE_MAX_STREAM_BYTES = '4';
  try {
    const { readStream } = await import('./cdp.mjs?stream-bound-test');
    const calls = [];
    const connection = {
      send: async (method, params) => {
        calls.push([method, params]);
        if (method === 'IO.read') return { data: '12345', eof: false };
        return {};
      },
    };
    await assert.rejects(readStream(connection, 'stream-1'), /IO stream exceeded 4 bytes/);
    assert.deepEqual(calls.at(-1), ['IO.close', { handle: 'stream-1' }]);
  } finally {
    if (previous === undefined) delete process.env.AO_PROBE_MAX_STREAM_BYTES;
    else process.env.AO_PROBE_MAX_STREAM_BYTES = previous;
  }
});
