import { afterEach, describe, expect, it, vi } from 'vitest';
import { backendClockSkew } from '../lib/transport/backendClock';
import { backendById } from '../lib/transport/backends';
import { HOME_BACKEND } from '../lib/transport/backendKey';
import type { TransportHello } from '../lib/transport/wsClient';

// The between-test steps return a module to its just-loaded state and
// break no other module's registration: the home backend registers its
// clock once, when its entry is made, and that entry outlives the steps.
const hello: TransportHello = {
  protocolVersion: 1, capabilities: [], backendId: '', backendName: '', serverTimeMs: 0, clockSkewMs: 4321,
  bundleId: '', bundleVersion: '', minShellBuild: 0,
};

describe('the home clock across tests', () => {
  afterEach(() => vi.restoreAllMocks());

  for (const run of ['first', 'second']) {
    it(`reads the home hello's skew in the ${run} test`, () => {
      vi.spyOn(backendById(HOME_BACKEND)!.client, 'getHello').mockReturnValue(hello);
      expect(backendClockSkew()).toBe(4321);
    });
  }
});
