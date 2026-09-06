import { expect, it } from 'vitest';
import { mergeComputerRoutes, normalizeComputerRoute, repairComputerRouteCandidates, MAX_COMPUTER_ROUTES } from './computerRoute';

it('keeps route origins separate from credentials and enforces HTTPS and exact pins', () => {
  expect(normalizeComputerRoute({ endpoint: ' https://GPU.Example:0443/ ' })).toEqual({ endpoint: 'https://gpu.example' });
  for (const endpoint of ['http://gpu', 'https://user:secret@gpu', 'https://gpu/path', 'https://gpu/?ticket=secret', 'https://gpu/?', 'https://gpu/#secret', 'https://gpu/#', 'https://gpu/a/..', 'https://gp\tu', 'https://gpu:0', 'https://gpu:65536', 'https://gpu:', 'https://gpu%00', 'https://[fe80::1%25en0]', 'https://gpu..example', 'https://-gpu', 'https://gpu_/', 'https://gpu/%2f']) {
    expect(normalizeComputerRoute({ endpoint }), endpoint).toBeNull();
  }
  expect(normalizeComputerRoute({ endpoint: 'https://gpu', certFingerprint: 'sha256:broken' })).toBeNull();
});

it('retains routes omitted by older hosts, prefers current trust, and bounds the catalog', () => {
  const old = { endpoint: 'https://gpu', certFingerprint: `sha256:${'a'.repeat(64)}` };
  const next = { ...old, certFingerprint: `sha256:${'b'.repeat(64)}` };
  expect(mergeComputerRoutes([old], undefined)).toEqual([old]);
  expect(mergeComputerRoutes([old], [next, next])).toEqual([next]);
  expect(mergeComputerRoutes([], Array.from({ length: 32 }, (_, n) => ({ endpoint: `https://computer-${n}` })))).toHaveLength(MAX_COMPUTER_ROUTES);
});

it('repairs addresses with existing private pins or the same WebPKI hostname', () => {
  const privateRoute = { endpoint: 'https://192.168.1.4', certFingerprint: `sha256:${'a'.repeat(64)}` };
  const publicRoute = { endpoint: 'https://gpu.tailnet.test' };
  expect(repairComputerRouteCandidates(privateRoute, [], 'https://192.168.1.8:8443')).toEqual([{ ...privateRoute, endpoint: 'https://192.168.1.8:8443' }]);
  expect(repairComputerRouteCandidates(publicRoute, [], 'https://gpu.tailnet.test:8443')).toEqual([{ endpoint: 'https://gpu.tailnet.test:8443' }]);
  expect(() => repairComputerRouteCandidates(publicRoute, [], 'https://another.test')).toThrow('saved computer trust');
  expect(repairComputerRouteCandidates(publicRoute, [privateRoute], 'https://192.168.1.8')).toEqual([{ ...privateRoute, endpoint: 'https://192.168.1.8' }]);
  expect(() => repairComputerRouteCandidates(privateRoute, [], 'http://192.168.1.8')).toThrow('HTTPS');
  const replacement = { ...privateRoute, certFingerprint: `sha256:${'b'.repeat(64)}` };
  expect(repairComputerRouteCandidates({ ...privateRoute, endpoint: 'https://192.168.1.4:443/' }, [replacement], 'https://192.168.1.8')).toEqual([{ ...replacement, endpoint: 'https://192.168.1.8' }]);
});
