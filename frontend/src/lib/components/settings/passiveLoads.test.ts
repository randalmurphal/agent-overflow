// The passive-load rule, applied to the settings sections.
//
// `stores/viewOnlyPassiveLoads.test.ts` sweeps the loaders that live in
// stores. These four don't: each section calls its RPCs from its own mount
// effect, which is why the store sweep was green while every one of them
// spent a refusal the moment a paired device opened Settings → Network
// (found by the harness, 2026-08-31 — `harness-remote-device-lifecycle`
// reads the same absence off the wire, end to end).
//
// One suite rather than a case per section, for the reason the store sweep
// gives: the rule is one rule and the failure it guards is a BURST.
//
// TWO AXES, and cases for both, because they refuse different sessions:
//
//   `host`            no session is ever granted it, so EVERY paired
//                     device is refused — full access included.
//   a named capability   view-only is refused; full access is not.
//
// A section gated on the wrong one is wrong in a direction a single
// view-only case cannot see, so the full-access device is asserted
// positively: it MUST still load what it holds.
//
// Every case asserts both directions. A guard that never fires would pass
// the negative while having broken the owner's own screen.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { resetRunMode } from '../../../test/runMode';
import { SCOPES } from '../../transport/scopes';
import DevicesSection from './DevicesSection.svelte';
import NetworkSection from './NetworkSection.svelte';
import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
import WSLSection, { resetWSLSectionCache } from './WSLSection.svelte';

/** A device paired with full access holds every grantable scope — not `host`. */
function pairFullAccess(): Promise<void> {
  return pairWithScopes(SCOPES);
}

function stubBindings() {
  return {
    network: setBindingMock('GetNetworkSettings', async () => ({
      bindAll: false,
      canonicalDomain: '',
      acmeDnsHook: [],
      externalCertFile: '',
      externalKeyFile: '',
      tls: {
        serving: 'self-signed',
        notAfter: 0,
        renewing: false,
        lastError: '',
        selfSignedFingerprint: '',
      },
      url: 'http://127.0.0.1:54321/?t=t',
      token: 't',
    })),
    overview: setBindingMock('GetAccessOverview', async () => ({ devices: [] })),
    endpoints: setBindingMock('ListRemoteEndpoints', async () => []),
    isWSL: setBindingMock('IsWSL', async () => true),
    distros: setBindingMock('ListWSLDistros', async () => [{ name: 'Ubuntu', default: true }]),
    distroPref: setBindingMock('GetWSLDistroPreference', async () => 'Ubuntu'),
  };
}

// The loads are fire-and-forget inside a mount effect, so a synchronous
// assertion would pass before the RPC that should not have happened had a
// chance to happen.
function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

describe('settings sections issue no passive RPC they were not granted', () => {
  let bindings: ReturnType<typeof stubBindings>;

  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    resetWSLSectionCache();
    bindings = stubBindings();
  });

  afterEach(() => {
    cleanup();
    resetToLocalPage();
    resetBindingMocks();
    resetRunMode();
    resetWSLSectionCache();
  });

  describe('NetworkSection — the bind preference is a fact about the machine', () => {
    it('loads it on the owner’s own screen', async () => {
      render(NetworkSection);
      await waitFor(() => expect(bindings.network).toHaveBeenCalled());
    });

    it('does not ask for it from a device paired with FULL access', async () => {
      await pairFullAccess();
      render(NetworkSection);
      await settle();
      expect(bindings.network).not.toHaveBeenCalled();
    });

    it('says why, rather than rendering a control that cannot work', async () => {
      await pairViewOnly();
      const { getByTestId, queryByLabelText } = render(NetworkSection);
      await settle();
      expect(bindings.network).not.toHaveBeenCalled();
      expect(getByTestId('network-section-local-only')).toBeTruthy();
      expect(queryByLabelText('Application URL')).toBeNull();
    });
  });

  describe('DevicesSection — pairing and revocation are access:admin', () => {
    it('loads the overview on the owner’s own screen', async () => {
      render(DevicesSection);
      await waitFor(() => expect(bindings.overview).toHaveBeenCalled());
    });

    it('loads it for a device paired with FULL access, which holds the scope', async () => {
      await pairFullAccess();
      render(DevicesSection);
      await waitFor(() => expect(bindings.overview).toHaveBeenCalled());
      // …and still does not reach for the machine fact alongside it.
      expect(bindings.network).not.toHaveBeenCalled();
    });

    it('does not ask a view-only device’s session for it', async () => {
      await pairViewOnly();
      const { getByTestId } = render(DevicesSection);
      await settle();
      expect(bindings.overview).not.toHaveBeenCalled();
      expect(bindings.network).not.toHaveBeenCalled();
      expect(getByTestId('devices-section-unavailable')).toBeTruthy();
    });
  });

  describe('RemoteEndpointsSection — saved launch targets are settings:write', () => {
    it('lists them on the owner’s own screen', async () => {
      render(RemoteEndpointsSection);
      await waitFor(() => expect(bindings.endpoints).toHaveBeenCalled());
    });

    it('lists them for a device paired with FULL access', async () => {
      await pairFullAccess();
      render(RemoteEndpointsSection);
      await waitFor(() => expect(bindings.endpoints).toHaveBeenCalled());
    });

    it('does not ask a view-only device’s session for them', async () => {
      await pairViewOnly();
      render(RemoteEndpointsSection);
      await settle();
      expect(bindings.endpoints).not.toHaveBeenCalled();
    });
  });

  describe('WSLSection — which distro the launcher boots is a fact about the machine', () => {
    it('detects and lists on the owner’s own screen', async () => {
      render(WSLSection);
      await waitFor(() => expect(bindings.distros).toHaveBeenCalled());
    });

    it('asks nothing at all from a device paired with FULL access', async () => {
      await pairFullAccess();
      render(WSLSection);
      await settle();
      // Including `IsWSL`: the section renders nothing either way, so the
      // detection probe is work with no reader.
      expect(bindings.isWSL).not.toHaveBeenCalled();
      expect(bindings.distros).not.toHaveBeenCalled();
      expect(bindings.distroPref).not.toHaveBeenCalled();
    });
  });
});
