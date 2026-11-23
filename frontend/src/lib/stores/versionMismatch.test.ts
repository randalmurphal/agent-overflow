// A call a computer does not have reads, on every surface, as the version
// mismatch it is: which computer, which side is older when both versions are
// known, and what to do. The sentence table is pinned here; the transport's
// nameless sentence is pinned to the Go side's in internal/rpcclient.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TransportError, type TransportHello } from '../transport/wsClient';
import {
  METHOD_UNAVAILABLE_SENTENCE,
  installMethodUnavailableWording,
  setMethodUnavailableDiagnosticsSink,
} from '../transport/methodUnavailable';
import { __resetRunModeForTest } from '../transport/runMode';
import { __resetHomeEndpointForTest, setHomeEndpoint } from '../transport/homeEndpoint';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { __resetPageHostForTest, setHostWindowFromBootstrap } from '../transport/pageHost';
import { installVersionMismatchWording, versionMismatchSentence, type VersionMismatchFacts } from './versionMismatch';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { errorReport, errorReportText } from '../utils/errorReport';
import { userFacingError } from '../utils/userFacingError';
import { errString } from '../utils/errors';
import { PAGE_VERSION } from '../pageVersion';

function facts(overrides: Partial<VersionMismatchFacts>): VersionMismatchFacts {
  return {
    computer: 'Studio',
    computerVersion: '0.0.19',
    computerServedPage: false,
    pageVersion: '0.0.20',
    page: 'window',
    source: 'served',
    server: 'Desk',
    serverVersion: '0.0.20',
    ...overrides,
  };
}

function hello(bundleVersion: string, backendName = ''): TransportHello {
  return {
    protocolVersion: 1, capabilities: [], backendId: '', backendName, serverTimeMs: 0, clockSkewMs: 0,
    bundleId: bundleVersion === '' ? '' : 'id', bundleVersion, minShellBuild: 0,
  };
}

describe('the version mismatch sentences', () => {
  it.each<[string, Partial<VersionMismatchFacts>, string]>([
    ['a paired computer older than the page', {},
      'Studio runs an older version of Agent Overflow than this window and does not support this. Update Agent Overflow on Studio.'],
    ['a paired computer newer, the page from a computer since updated', { computerVersion: '0.0.21', serverVersion: '0.0.21', page: 'page' },
      'Studio runs a newer version of Agent Overflow than this page and does not support this. Reload the page.'],
    ['a paired computer newer, the page from a computer not updated', { computerVersion: '0.0.21', page: 'page' },
      'Studio runs a newer version of Agent Overflow than this page and does not support this. Update Agent Overflow on Desk, then reload the page.'],
    ['a paired computer newer, the page from this computer\'s own installation', { computerVersion: '0.0.21', source: 'local' },
      'Studio runs a newer version of Agent Overflow than this window and does not support this. Update Agent Overflow on this computer.'],
    ['a paired computer newer, the page the phone app\'s bundle', { computerVersion: '0.0.21', source: 'device', page: 'app' },
      'Studio runs a newer version of Agent Overflow than this app and does not support this. Update Agent Overflow on this device.'],
    ['a paired computer of unknown version', { computerVersion: '' },
      'Studio runs a different version of Agent Overflow than this window and does not support this. Update Agent Overflow so both run the same version.'],
    ['a paired computer reporting the page\'s own version', { computerVersion: '0.0.20' },
      'Studio runs a different version of Agent Overflow than this window and does not support this. Update Agent Overflow so both run the same version.'],
    ['the home computer, updated under an open page', { computerServedPage: true, computerVersion: '0.0.21', page: 'page' },
      'Studio now runs a newer version of Agent Overflow than this page. Reload the page.'],
    ['the home computer, downgraded under an open window', { computerServedPage: true },
      'Studio now runs an older version of Agent Overflow than this window. Reload the window.'],
    ['the home computer, version unknown', { computerServedPage: true, computerVersion: 'not-a-version', page: 'page' },
      'Studio now runs a different version of Agent Overflow than this page. Reload the page.'],
  ])('%s', (_case, overrides, sentence) => {
    expect(versionMismatchSentence(facts(overrides))).toBe(sentence);
  });
});

describe('a refused call as every surface reads it', () => {
  beforeEach(() => {
    setBackendIdentityFromBootstrap('11111111-2222-4333-8444-555555555555', 'g1', 'Desk');
  });

  afterEach(() => {
    resetStagedBackends();
    __setTransportHelloForTest(null);
    __resetHomeEndpointForTest();
    __resetRunModeForTest();
    window.history.replaceState(null, '', '/');
    __resetPageHostForTest();
    setHostWindowFromBootstrap(true);
    installVersionMismatchWording();
    setMethodUnavailableDiagnosticsSink(null);
  });

  it('names the paired computer and the side to update, through every reader of the error', () => {
    const studio = stageBackend({ id: 'studio', name: 'Studio' });
    studio.setHello(hello('0.0.1'));
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'studio' });
    const sentence = 'Studio runs an older version of Agent Overflow than this page and does not support this. Update Agent Overflow on Studio.';
    expect(err.message).toBe(sentence);
    expect(userFacingError(err)).toBe(sentence);
    expect(errString(err)).toBe(sentence);
    expect(`Failed to delete: ${err.message}`).not.toContain('method not registered');
  });

  it('shows a computer name holding a colon whole', () => {
    stageBackend({ id: 'lab', name: 'Lab: GPU box' }).setHello(hello('0.0.1'));
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'lab' });
    expect(userFacingError(err)).toBe('Lab: GPU box runs an older version of Agent Overflow than this page and does not support this. Update Agent Overflow on Lab: GPU box.');
  });

  it('tells a page its own computer was updated under it to reload', () => {
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe('Desk now runs a newer version of Agent Overflow than this page. Reload the page.');
  });

  it('calls the desktop window a window', () => {
    window.history.replaceState(null, '', '/?host=webview');
    __resetPageHostForTest();
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe('Desk now runs a newer version of Agent Overflow than this window. Reload the window.');
  });

  it('does not offer a reload to a page its home computer did not serve', () => {
    window.history.replaceState(null, '', '/?mode=frontend');
    __resetRunModeForTest();
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe('Desk runs a newer version of Agent Overflow than this page and does not support this. Update Agent Overflow on this computer.');
  });

  it('words the phone app\'s page as the device to update', () => {
    setHomeEndpoint('https://desk.example:4443');
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe('Desk runs a newer version of Agent Overflow than this page and does not support this. Update Agent Overflow on this device.');
  });

  it('keeps the code and the wire text for Details and a copied report', () => {
    const err = new TransportError('method_not_found', 'method not registered');
    const report = errorReport(err, { context: 'Failed to delete' });
    expect(report.code).toBe('method_not_found');
    expect(report.reason).toBe(err.message);
    expect(report.chain).toEqual([err.message, 'method not registered']);
    const text = errorReportText(report);
    expect(text).toContain('- code: method_not_found');
    expect(text).toContain('2. method not registered');
  });

  it('says the nameless sentence when no wording is installed', () => {
    installMethodUnavailableWording(null);
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'studio' });
    expect(err.message).toBe(METHOD_UNAVAILABLE_SENTENCE);
    expect(err.message).toBe('The computer this reached runs a different version of Agent Overflow and does not support this. Update Agent Overflow so both run the same version.');
  });

  it('says the nameless sentence for a computer that has detached', () => {
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'gone' });
    expect(err.message).toBe(METHOD_UNAVAILABLE_SENTENCE);
  });

  it('reports a wording that throws and still says the nameless sentence', () => {
    const sink = vi.fn();
    setMethodUnavailableDiagnosticsSink(sink);
    installMethodUnavailableWording(() => { throw new Error('no names'); });
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe(METHOD_UNAVAILABLE_SENTENCE);
    expect(sink).toHaveBeenCalledWith('transport: version mismatch wording failed', 'no names');
  });

  it('reads the version this build compiled in', () => {
    expect(PAGE_VERSION).toMatch(/^\d+\.\d+\.\d+/);
  });
});
