// The webview-hosted page's half of the ticket handover. Everything here
// is about ORDER: the host's injection and this page's boot code race,
// and neither side can be made to win, so both landings have to work and
// a repeat has to change nothing. The Go half is internal/pagehost and
// internal/uiwindow.DeliverPageTicket.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetPageHostForTest,
  awaitInjectedPageTicket,
  clearInjectedPageTicket,
  isWebviewHosted,
  PageTicketUndeliveredError,
  readInjectedPageTicket,
} from './pageHost';

const TICKET_GLOBAL = '__aoPageTicket';
const TICKET_EVENT = 'ao:page-ticket';
const HOST_READY_MESSAGE = 'wails:runtime:ready';

type MutableWindow = Record<string, unknown>;

// The host globals are written by script the host evaluates, so the test
// has to reach the same untyped slots.
function hostGlobals(): MutableWindow {
  return window as unknown as MutableWindow;
}

function setPageSearch(search: string): void {
  window.history.replaceState(null, '', window.location.pathname + search);
}

// deliver is exactly what internal/pagehost.DeliveryScript evaluates: the
// global, then the event. Spelled here rather than imported so a change
// on either side of the contract fails a test instead of tracking itself.
function deliver(ticket: string): void {
  hostGlobals()[TICKET_GLOBAL] = ticket;
  window.dispatchEvent(new Event(TICKET_EVENT));
}

beforeEach(() => {
  __resetPageHostForTest();
});

afterEach(() => {
  __resetPageHostForTest();
  setPageSearch('');
  delete hostGlobals()._wails;
  delete hostGlobals().chrome;
  vi.useRealTimers();
});

describe('isWebviewHosted', () => {
  it('is false for a page with no host marker, which is every browser', () => {
    setPageSearch('?cid=desktop-1&t=ticket-1');
    expect(isWebviewHosted()).toBe(false);
  });

  it('is true only for the exact marker the Go side stamps', () => {
    setPageSearch('?host=webview&mode=client&cid=desktop-1');
    expect(isWebviewHosted()).toBe(true);
  });

  it('rejects another host value rather than treating any host as a window', () => {
    setPageSearch('?host=browser');
    expect(isWebviewHosted()).toBe(false);
  });
});

describe('awaitInjectedPageTicket', () => {
  it('resolves from the global when the injection landed first', async () => {
    // The host evaluated its script before this module ran. There is no
    // event left to hear, so the global is the only record of it.
    hostGlobals()[TICKET_GLOBAL] = 'ticket-early';
    await expect(awaitInjectedPageTicket()).resolves.toBe('ticket-early');
  });

  it('resolves from the event when the injection lands after the wait', async () => {
    const pending = awaitInjectedPageTicket();
    deliver('ticket-late');
    await expect(pending).resolves.toBe('ticket-late');
  });

  it('is idempotent under a repeated delivery', async () => {
    // The host re-injects on every announcement, and the SPA
    // re-announces while waiting, so a second delivery is ordinary.
    const pending = awaitInjectedPageTicket();
    deliver('ticket-1');
    deliver('ticket-1');
    await expect(pending).resolves.toBe('ticket-1');
    // A delivery arriving after the wait settled must not throw or leave
    // a listener behind; the next wait simply reads the newest value.
    deliver('ticket-2');
    await expect(awaitInjectedPageTicket()).resolves.toBe('ticket-2');
  });

  it('keeps waiting through a dispatch that carries no ticket', async () => {
    vi.useFakeTimers();
    const pending = awaitInjectedPageTicket(1_000);
    const settled = vi.fn();
    void pending.then(settled, settled);
    window.dispatchEvent(new Event(TICKET_EVENT));
    await Promise.resolve();
    expect(settled).not.toHaveBeenCalled();
    deliver('ticket-real');
    await expect(pending).resolves.toBe('ticket-real');
  });

  it('rejects at the deadline rather than hanging the boot', async () => {
    vi.useFakeTimers();
    const pending = awaitInjectedPageTicket(1_000);
    const caught = pending.catch((err: unknown) => err);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await caught).toBeInstanceOf(PageTicketUndeliveredError);
  });

  it('stops announcing once the deadline passed', async () => {
    vi.useFakeTimers();
    const invoke = vi.fn();
    hostGlobals()._wails = { invoke };
    const pending = awaitInjectedPageTicket(1_000);
    const caught = pending.catch(() => undefined);
    await vi.advanceTimersByTimeAsync(1_000);
    await caught;
    const announcedWhileWaiting = invoke.mock.calls.length;
    await vi.advanceTimersByTimeAsync(5_000);
    expect(invoke.mock.calls.length).toBe(announcedWhileWaiting);
  });
});

describe('announcing to the host', () => {
  it('uses the Wails bridge when the host has installed it', async () => {
    const invoke = vi.fn();
    hostGlobals()._wails = { invoke };
    const pending = awaitInjectedPageTicket();
    expect(invoke).toHaveBeenCalledWith(HOST_READY_MESSAGE);
    deliver('ticket-1');
    await pending;
  });

  it('falls back to the platform bridge, which exists before the page loads', async () => {
    // The Wails bridge is installed at load-finished; the engine's own
    // channel is there from document creation, so reaching it directly
    // is what lets the connection start without waiting for `load`.
    const postMessage = vi.fn();
    hostGlobals().chrome = { webview: { postMessage } };
    const pending = awaitInjectedPageTicket();
    expect(postMessage).toHaveBeenCalledWith(HOST_READY_MESSAGE);
    deliver('ticket-1');
    await pending;
  });

  it('waits for the host bridge when neither carrier exists yet', async () => {
    const pending = awaitInjectedPageTicket();
    const invoke = vi.fn();
    hostGlobals()._wails = { invoke };
    window.dispatchEvent(new Event('wails:runtime-config-ready'));
    expect(invoke).toHaveBeenCalledWith(HOST_READY_MESSAGE);
    deliver('ticket-1');
    await pending;
  });
});

describe('clearInjectedPageTicket', () => {
  it('forgets a delivered ticket so the next wait asks for a fresh one', async () => {
    deliver('ticket-stale');
    expect(readInjectedPageTicket()).toBe('ticket-stale');
    clearInjectedPageTicket();
    expect(readInjectedPageTicket()).toBe('');

    const pending = awaitInjectedPageTicket();
    deliver('ticket-fresh');
    await expect(pending).resolves.toBe('ticket-fresh');
  });
});
