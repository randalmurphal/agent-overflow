// The passive-load rule, applied to the settings sections.
//
// `stores/viewOnlyPassiveLoads.test.ts` sweeps the loaders that live in
// stores. These don't: each section calls its RPCs from its own mount
// effect, which is why the store sweep was green while every one of them
// spent a refusal the moment a paired device opened Settings → Remote access
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
// positively: it MUST still load what it holds. NetworkSection is the
// worked example in both directions: it was gated on `host`, which
// refused the owner's own phone from a screen that exists to manage
// remote access, and it is gated on `access:admin` now.
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
import WSLSection, { resetWSLSectionCache } from './WSLSection.svelte';
import NotificationsSection from './NotificationsSection.svelte';
import SystemsSection from './SystemsSection.svelte';
import { __resetSystemsForTest } from '../../stores/systems.svelte';

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
      tailnetEnabled: false,
      tailnetControlUrl: '',
      tailnet: {
        running: false,
        state: '',
        authUrl: '',
        dnsName: '',
        ips: [],
        url: '',
        https: false,
        hasState: false,
        lastError: '',
      },
      url: 'http://127.0.0.1:54321/?t=t',
      token: 't',
    })),
    // The tailnet's own RPC is host-scoped, and it DELETES this backend's
    // node identity — so it is here to assert it never fires from a mount,
    // on any screen, including the admin device that may now read beside
    // it.
    forgetTailnet: setBindingMock('ForgetTailnetNode', async () => {
      throw new Error('ForgetTailnetNode must never be called by a passive load');
    }),
    overview: setBindingMock('GetAccessOverview', async () => ({ devices: [] })),
    // The passkeys block lives inside DevicesSection's granted branch, so
    // it is the same passive load one level down — and mounting it there
    // rather than gating it again is exactly the arrangement that has to
    // be asserted rather than assumed.
    passkeys: setBindingMock('ListPasskeys', async () => []),
    isWSL: setBindingMock('IsWSL', async () => true),
    distros: setBindingMock('ListWSLDistros', async () => [{ name: 'Ubuntu', default: true }]),
    distroPref: setBindingMock('GetWSLDistroPreference', async () => 'Ubuntu'),
    systems: setBindingMock('ListBackends', async () => []),
    // The phone-push block reads and writes with `access:admin` (the writes
    // step-up gated on top). Both are stubbed so the sweep can assert that
    // the read fires where it is granted and the write never fires from a
    // mount at all.
    pushStatus: setBindingMock('GetPushSenderStatus', async () => ({
      configured: false,
      projectId: '',
      clientEmail: '',
      lastError: '',
      registeredDevices: 0,
    })),
    setPushCredential: setBindingMock('SetPushSenderCredential', async () => {
      throw new Error('SetPushSenderCredential must never be called by a passive load');
    }),
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
    __resetSystemsForTest();
    bindings = stubBindings();
  });

  afterEach(() => {
    cleanup();
    resetToLocalPage();
    resetBindingMocks();
    resetRunMode();
    resetWSLSectionCache();
  });

  describe('NetworkSection — managing exposure is access:admin', () => {
    it('loads it on the owner’s own screen', async () => {
      render(NetworkSection);
      await waitFor(() => expect(bindings.network).toHaveBeenCalled());
      // The tailnet block rides that one read. Nothing about it is a
      // second passive call, and its destructive action is not one at all.
      expect(bindings.forgetTailnet).not.toHaveBeenCalled();
    });

    it('loads it for a device paired with FULL access, which holds the scope', async () => {
      await pairFullAccess();
      render(NetworkSection);
      await waitFor(() => expect(bindings.network).toHaveBeenCalled());
      // Reading the settings is the grant. Deleting the node identity is
      // not, and no mount may reach for it whatever the device holds.
      expect(bindings.forgetTailnet).not.toHaveBeenCalled();
    });

    it('says why, rather than rendering a control that cannot work', async () => {
      await pairViewOnly();
      const { getByTestId, queryByLabelText, queryByTestId } = render(NetworkSection);
      await settle();
      expect(bindings.network).not.toHaveBeenCalled();
      expect(bindings.forgetTailnet).not.toHaveBeenCalled();
      expect(getByTestId('network-section-local-only')).toBeTruthy();
      expect(queryByLabelText('Application URL')).toBeNull();
      expect(queryByTestId('network-tailnet-editor')).toBeNull();
    });
  });

  describe('DevicesSection — pairing and revocation are access:admin', () => {
    it('loads the overview on the owner’s own screen', async () => {
      render(DevicesSection);
      await waitFor(() => expect(bindings.overview).toHaveBeenCalled());
      await waitFor(() => expect(bindings.passkeys).toHaveBeenCalled());
    });

    it('loads it for a device paired with FULL access, which holds the scope', async () => {
      await pairFullAccess();
      render(DevicesSection);
      await waitFor(() => expect(bindings.overview).toHaveBeenCalled());
      await waitFor(() => expect(bindings.passkeys).toHaveBeenCalled());
      // …and still does not reach for the section beside it, which holds
      // the same grant and loads itself.
      expect(bindings.network).not.toHaveBeenCalled();
    });

    it('does not ask a view-only device’s session for it', async () => {
      await pairViewOnly();
      const { getByTestId } = render(DevicesSection);
      await settle();
      expect(bindings.overview).not.toHaveBeenCalled();
      expect(bindings.network).not.toHaveBeenCalled();
      // The block never mounted, so its load never fired. That is the
      // whole reason it is nested inside the granted branch rather than
      // carrying a copy of the check.
      expect(bindings.passkeys).not.toHaveBeenCalled();
      expect(getByTestId('devices-section-unavailable')).toBeTruthy();
    });
  });

  describe('SystemsSection — the attached-machine list is host', () => {
    it('loads it on the owner’s own screen', async () => {
      render(SystemsSection);
      await waitFor(() => expect(bindings.systems).toHaveBeenCalled());
    });

    it('asks nothing from a device paired with FULL access, and says why', async () => {
      await pairFullAccess();
      const { getByTestId } = render(SystemsSection);
      await settle();
      expect(bindings.systems).not.toHaveBeenCalled();
      expect(getByTestId('systems-section-unavailable')).toBeTruthy();
    });
  });

  describe('NotificationsSection — the phone-push block is one access:admin surface', () => {
    it('loads the push status on the owner’s own screen, and offers the credential field', async () => {
      const { getByLabelText } = render(NotificationsSection);
      await waitFor(() => expect(bindings.pushStatus).toHaveBeenCalled());
      expect(getByLabelText('Firebase service account key')).toBeTruthy();
      expect(bindings.setPushCredential).not.toHaveBeenCalled();
    });

    it('loads it for a device paired with FULL access, and offers the field there too', async () => {
      await pairFullAccess();
      const { getByLabelText } = render(NotificationsSection);
      await waitFor(() => expect(bindings.pushStatus).toHaveBeenCalled());
      // The write is `access:admin` + step-up, the same posture as minting
      // a pairing link: reachable from a paired admin device, proved per
      // call. The machine this key most needs installing on is the serve
      // host nobody sits at, so a host-only field would guard nothing and
      // strand that case.
      expect(getByLabelText('Firebase service account key')).toBeTruthy();
      expect(bindings.setPushCredential).not.toHaveBeenCalled();
    });

    it('asks nothing from a view-only device, and still renders the toggles it owns', async () => {
      await pairViewOnly();
      const { getByTestId, queryByTestId } = render(NotificationsSection);
      await settle();
      expect(bindings.pushStatus).not.toHaveBeenCalled();
      // The section itself is not gated: these preferences describe the
      // screen the person is on, and every screen has them.
      expect(getByTestId('settings-notifications-section')).toBeTruthy();
      expect(queryByTestId('settings-phone-push')).toBeNull();
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
