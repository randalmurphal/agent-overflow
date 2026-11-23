// A refusal by the computer's dispatcher (a call it does not have, or
// arguments it cannot read) reads, on every surface, as what happened, what
// the page knows about the two versions, and what that knowledge supports
// doing. The sentence table is pinned here; the transport's nameless
// sentences are pinned to the Go side's in internal/dispatchrefusal.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TransportError, type TransportHello } from '../transport/wsClient';
import {
  UNREADABLE_SENTENCE,
  UNSUPPORTED_SENTENCE,
  installDispatchRefusalWording,
  setDispatchRefusalDiagnosticsSink,
} from '../transport/dispatchRefusal';
import { __resetRunModeForTest } from '../transport/runMode';
import { __resetHomeEndpointForTest, setHomeEndpoint } from '../transport/homeEndpoint';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { __resetPageHostForTest, setHostWindowFromBootstrap } from '../transport/pageHost';
import { installVersionMismatchWording, versionMismatchSentence, type VersionMismatchFacts } from './versionMismatch';
import { __resetBundleNoticeForTest, noteBundleReady } from './bundleNotice.svelte';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { errorReport, errorReportText } from '../utils/errorReport';
import { userFacingError } from '../utils/userFacingError';
import { errString } from '../utils/errors';
import { PAGE_VERSION } from '../pageVersion';

function facts(overrides: Partial<VersionMismatchFacts>): VersionMismatchFacts {
  return {
    kind: 'unsupported',
    computer: 'Studio',
    computerVersion: '0.0.19',
    computerServedPage: false,
    pageVersion: '0.0.20',
    page: 'window',
    source: 'served',
    server: () => ({ name: 'Desk', version: '0.0.20' }),
    stagedVersion: '',
    ...overrides,
  };
}

function served(version: string): Pick<VersionMismatchFacts, 'server'> {
  return { server: () => ({ name: 'Desk', version }) };
}

function hello(bundleVersion: string, backendName = ''): TransportHello {
  return {
    protocolVersion: 1, capabilities: [], backendId: '', backendName, serverTimeMs: 0, clockSkewMs: 0,
    bundleId: bundleVersion === '' ? '' : 'id', bundleVersion, minShellBuild: 0,
  };
}

describe('the sentences for a refusal', () => {
  it.each<[string, Partial<VersionMismatchFacts>, string]>([
    ['a paired computer older than the page', {},
      'Studio does not support this action. Studio runs Agent Overflow 0.0.19, older than this window (0.0.20). Update Agent Overflow on Studio.'],
    ['arguments a paired computer older than the page cannot read', { kind: 'unreadable' },
      'Studio could not read this request. Studio runs Agent Overflow 0.0.19, older than this window (0.0.20). Update Agent Overflow on Studio.'],
    ['a paired computer newer, the page from a computer since updated', { computerVersion: '0.0.21', ...served('0.0.21'), page: 'page' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this page (0.0.20). Reload the page.'],
    ['a paired computer newer, the page from a computer not updated', { computerVersion: '0.0.21', page: 'page' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this page (0.0.20). Update Agent Overflow on Desk, then reload the page.'],
    ['a paired computer newer, the page from a computer of unknown version', { computerVersion: '0.0.21', ...served(''), page: 'page' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this page (0.0.20). If Desk runs 0.0.21 or newer, reload the page. If not, update Agent Overflow on Desk, then reload the page.'],
    ['a paired computer newer, the page from this computer\'s own installation', { computerVersion: '0.0.21', source: 'local' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this window (0.0.20). Update Agent Overflow on this computer.'],
    ['a paired computer newer, nothing newer staged on the phone', { computerVersion: '0.0.21', source: 'device', page: 'app' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this app (0.0.20). Install a newer version of the Agent Overflow app on this device.'],
    ['a paired computer newer, an older release staged on the phone', { computerVersion: '0.0.22', source: 'device', page: 'app', stagedVersion: '0.0.21' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.22, newer than this app (0.0.20). Install a newer version of the Agent Overflow app on this device.'],
    ['a paired computer newer, its release staged on the phone', { computerVersion: '0.0.21', source: 'device', page: 'app', stagedVersion: '0.0.21' },
      'Studio does not support this action. Studio runs Agent Overflow 0.0.21, newer than this app (0.0.20). Close and reopen the app to load 0.0.21.'],
    ['a paired computer of unknown version', { computerVersion: '' },
      'Studio does not support this action. This window cannot tell which version of Agent Overflow Studio runs. Check that Studio and this window run the same version, and update whichever is older.'],
    ['a paired computer reporting the page\'s own version', { computerVersion: '0.0.20' },
      'Studio does not support this action. Studio runs the same version of Agent Overflow as this window (0.0.20). Copy the details of this error and report it.'],
    ['the home computer, updated under an open page', { computerServedPage: true, computerVersion: '0.0.21', page: 'page' },
      'Studio does not support this action. Studio now runs Agent Overflow 0.0.21, newer than this page (0.0.20). Reload the page.'],
    ['the home computer, downgraded under an open window', { computerServedPage: true },
      'Studio does not support this action. Studio now runs Agent Overflow 0.0.19, older than this window (0.0.20). Reload the window.'],
    ['the home computer, version unknown', { computerServedPage: true, computerVersion: 'not-a-version', page: 'page' },
      'Studio does not support this action. This page cannot tell which version of Agent Overflow Studio runs. Check that Studio and this page run the same version, and update whichever is older. If Studio was updated while this page was open, reload the page.'],
    ['the home computer, the page\'s own version', { computerServedPage: true, computerVersion: '0.0.20', page: 'page' },
      'Studio does not support this action. Studio runs the same version of Agent Overflow as this page (0.0.20). Copy the details of this error and report it.'],
  ])('%s', (_case, overrides, sentence) => {
    expect(versionMismatchSentence(facts(overrides))).toBe(sentence);
    // A person's toast on a release before this one keeps the text after
    // its last colon, so a sentence holds none of its own.
    expect(sentence.replaceAll('Studio', '').replaceAll('Desk', '')).not.toContain(':');
  });

  it('asks for the computer that served the page only when the remedy needs it', () => {
    const server = vi.fn(() => ({ name: 'Desk', version: '0.0.20' }));
    versionMismatchSentence(facts({ server }));
    versionMismatchSentence(facts({ server, computerVersion: '' }));
    versionMismatchSentence(facts({ server, computerVersion: '0.0.20' }));
    expect(server).not.toHaveBeenCalled();
    versionMismatchSentence(facts({ server, computerVersion: '0.0.21' }));
    expect(server).toHaveBeenCalledOnce();
  });
});

describe('a refused call as every surface reads it', () => {
  beforeEach(() => {
    setBackendIdentityFromBootstrap('11111111-2222-4333-8444-555555555555', 'g1', 'Desk');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    resetStagedBackends();
    __setTransportHelloForTest(null);
    __resetHomeEndpointForTest();
    __resetRunModeForTest();
    __resetBundleNoticeForTest();
    window.history.replaceState(null, '', '/');
    __resetPageHostForTest();
    setHostWindowFromBootstrap(true);
    installVersionMismatchWording();
    setDispatchRefusalDiagnosticsSink(null);
  });

  it('names the paired computer and the side to update, through every reader of the error', () => {
    stageBackend({ id: 'studio', name: 'Studio' }).setHello(hello('0.0.1'));
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'studio' });
    const sentence = `Studio does not support this action. Studio runs Agent Overflow 0.0.1, older than this page (${PAGE_VERSION}). Update Agent Overflow on Studio.`;
    expect(err.message).toBe(sentence);
    expect(userFacingError(err)).toBe(sentence);
    expect(errString(err)).toBe(sentence);
    expect(`Failed to delete: ${err.message}`).not.toContain('method not registered');
  });

  it('words arguments the computer could not read, from a computer that still sends the old text', () => {
    stageBackend({ id: 'studio', name: 'Studio' }).setHello(hello('0.0.1'));
    const err = new TransportError('bad_params', 'App.GitPush: expected 1 params, got 2', { backend: 'studio' });
    expect(userFacingError(err)).toBe(`Studio could not read this request. Studio runs Agent Overflow 0.0.1, older than this page (${PAGE_VERSION}). Update Agent Overflow on Studio.`);
    expect(err.cause).toBe('App.GitPush: expected 1 params, got 2');
  });

  it('leaves a refusal a method returned as the method said it', () => {
    const detail = { ref: 'ref-1', method: 'ReadThreadRemoteLog', at: 1, chain: [] };
    const forwarded = new TransportError('method_not_found', 'Could not read the log of "build" on Nexus: method not registered', { detail });
    expect(forwarded.message).toBe('Could not read the log of "build" on Nexus: method not registered');
    const invalid = new TransportError('bad_params', 'Use a label of 80 characters or fewer.', { detail });
    expect(userFacingError(invalid)).toBe('Use a label of 80 characters or fewer.');
  });

  it('shows a computer name holding a colon whole', () => {
    stageBackend({ id: 'lab', name: 'Lab: GPU box' }).setHello(hello('0.0.1'));
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'lab' });
    expect(userFacingError(err)).toBe(`Lab: GPU box does not support this action. Lab: GPU box runs Agent Overflow 0.0.1, older than this page (${PAGE_VERSION}). Update Agent Overflow on Lab: GPU box.`);
  });

  it('tells a page its own computer was updated under it to reload', () => {
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe(`Desk does not support this action. Desk now runs Agent Overflow 999.0.0, newer than this page (${PAGE_VERSION}). Reload the page.`);
  });

  it('calls the desktop window a window', () => {
    window.history.replaceState(null, '', '/?host=webview');
    __resetPageHostForTest();
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe(`Desk does not support this action. Desk now runs Agent Overflow 999.0.0, newer than this window (${PAGE_VERSION}). Reload the window.`);
  });

  it('does not offer a reload to a page its home computer did not serve', () => {
    window.history.replaceState(null, '', '/?mode=frontend');
    __resetRunModeForTest();
    __setTransportHelloForTest(hello('999.0.0'));
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe(`Desk does not support this action. Desk runs Agent Overflow 999.0.0, newer than this page (${PAGE_VERSION}). Update Agent Overflow on this computer.`);
  });

  it('takes the phone\'s remedy from what the bundle updater has staged', () => {
    setHomeEndpoint('https://desk.example:4443');
    __setTransportHelloForTest(hello('999.0.0'));
    const what = `Desk does not support this action. Desk runs Agent Overflow 999.0.0, newer than this page (${PAGE_VERSION}).`;
    expect(new TransportError('method_not_found', 'method not registered').message)
      .toBe(`${what} Install a newer version of the Agent Overflow app on this device.`);
    noteBundleReady('999.0.0');
    expect(new TransportError('method_not_found', 'method not registered').message)
      .toBe(`${what} Close and reopen the app to load 999.0.0.`);
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

  it('says the nameless sentences when no wording is installed', () => {
    installDispatchRefusalWording(null);
    expect(new TransportError('method_not_found', 'method not registered', { backend: 'studio' }).message).toBe(UNSUPPORTED_SENTENCE);
    expect(new TransportError('bad_params', 'expected 1 params, got 2').message).toBe(UNREADABLE_SENTENCE);
  });

  it('says the nameless sentence for a computer that has detached', () => {
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'gone' });
    expect(err.message).toBe(UNSUPPORTED_SENTENCE);
  });

  it('reports a wording that throws and still says the nameless sentence', () => {
    const sink = vi.fn();
    setDispatchRefusalDiagnosticsSink(sink);
    installDispatchRefusalWording(() => { throw new Error('no names'); });
    const err = new TransportError('method_not_found', 'method not registered');
    expect(err.message).toBe(UNSUPPORTED_SENTENCE);
    expect(sink).toHaveBeenCalledWith('transport: dispatch refusal wording failed', 'no names');
  });

  it('words a refusal only when its message is read, once', () => {
    const wording = vi.fn(() => 'worded');
    installDispatchRefusalWording(wording);
    const err = new TransportError('method_not_found', 'method not registered');
    expect(wording).not.toHaveBeenCalled();
    expect(err.message).toBe('worded');
    expect(err.message).toBe('worded');
    expect(wording).toHaveBeenCalledOnce();
  });

  // Right after a computer's identity changes, the sidebar's display names
  // are stale and rebuilding them reads the remembered identities from
  // storage. Wording a refusal looks its one name up instead.
  it('names the computer from memory, without reading storage', () => {
    stageBackend({ id: 'studio', name: 'Studio' }).setHello(hello('0.0.1'));
    setBackendIdentityFromBootstrap('11111111-2222-4333-8444-555555555555', 'g2', 'Desk');
    const local = vi.spyOn(localStorage, 'getItem');
    const session = vi.spyOn(sessionStorage, 'getItem');
    const err = new TransportError('method_not_found', 'method not registered', { backend: 'studio' });
    expect(err.message).toContain('Studio does not support this action.');
    expect(local).not.toHaveBeenCalled();
    expect(session).not.toHaveBeenCalled();
  });

  it('reads the version this build compiled in', () => {
    expect(PAGE_VERSION).toMatch(/^\d+\.\d+\.\d+/);
  });
});
