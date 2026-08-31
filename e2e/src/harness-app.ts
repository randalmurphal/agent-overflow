import { type ChildProcess } from 'node:child_process';
import { rm } from 'node:fs/promises';

import type { Page } from '@playwright/test';

import {
  type ProcessIdentity,
  type ProcessGroupMemberProof,
  type ProcessTreeProof,
} from './harness-process.ts';
import { HarnessWatchdog } from './harness-watchdog.ts';
import { boundedCleanup } from './harness-cleanup.ts';
import { terminateHarness } from './harness-teardown.ts';
import type { HarnessBootstrap } from './harness-types.ts';

// HarnessUIQuery may legitimately wait the backend's full 10s when this
// client is connected but no frontend bridge is attached. Keep the client
// deadline above that server deadline so the backend's diagnostic error wins
// instead of being replaced by a generic client timeout.
const RPC_TIMEOUT_MS = 15_000;

interface WireEvent {
  channel: string;
  seq: number;
  data: unknown;
  /** Set once a waitForEvent call has accepted this event. */
  consumed?: boolean;
}

interface PendingRpc {
  resolve: (result: unknown) => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

/** A live harness-mode backend plus one WS connection to it. */
export class HarnessApp {
  readonly bootstrap: HarnessBootstrap;
  /**
   * The page URL the backend printed at boot. Its one-time ticket is
   * spent by the first page that loads it, so this is the URL's identity
   * (origin, page marker, client id) rather than something to navigate
   * to twice: call `open` / `pageURL` to navigate.
   */
  readonly url: string;

  private child: ChildProcess;
  private removeDataDir?: string;
  private ws?: WebSocket;
  private nextId = 1;
  private pending = new Map<string, PendingRpc>();
  private eventLog: WireEvent[] = [];
  private eventWaiters = new Set<(ev: WireEvent) => void>();
  private closed = false;
  private readonly watchdog: HarnessWatchdog;
  private processGroupMemberProof?: ProcessGroupMemberProof;
  private processTreeProof?: ProcessTreeProof;
  private processIdentity?: ProcessIdentity;
  private teardownComplete = false;

  constructor(
    child: ChildProcess,
    bootstrap: HarnessBootstrap,
    removeDataDir?: string,
    memberProof?: ProcessGroupMemberProof,
    treeProof?: ProcessTreeProof,
    identity?: ProcessIdentity,
  ) {
    this.child = child;
    this.bootstrap = bootstrap;
    this.url = bootstrap.url;
    this.removeDataDir = removeDataDir;
    this.processGroupMemberProof = memberProof;
    this.processTreeProof = treeProof;
    this.processIdentity = identity;
    this.watchdog = new HarnessWatchdog({
      child,
      dataRoot: bootstrap.dataRoot,
      dataDir: bootstrap.dataDir,
      isClosed: () => this.closed,
      markClosed: () => {
        this.closed = true;
      },
      shutdown: () => this.rpc('HarnessShutdown'),
      closeSocket: () => this.ws?.close(),
      socketOpen: () => this.ws?.readyState === WebSocket.OPEN,
    });
  }

  async connect(): Promise<void> {
    const ws = new WebSocket(
      `ws://127.0.0.1:${this.bootstrap.port}/ws?token=${encodeURIComponent(this.bootstrap.token)}`,
    );
    this.ws = ws;
    ws.addEventListener('message', (msg) => this.onFrame(String(msg.data)));
    ws.addEventListener('close', () => {
      if (this.closed) return;
      const err = new Error('harness WS closed unexpectedly');
      for (const p of this.pending.values()) {
        clearTimeout(p.timer);
        p.reject(err);
      }
      this.pending.clear();
    });
    await new Promise<void>((resolve, reject) => {
      ws.addEventListener('open', () => resolve(), { once: true });
      ws.addEventListener('error', () => reject(new Error('harness WS failed to connect')), {
        once: true,
      });
    });
  }

  /**
   * Returns a page URL carrying a freshly minted one-time ticket, which
   * the page exchanges for its HttpOnly session cookie on first contact.
   *
   * Every navigation needs one of its own: a Playwright context is a
   * fresh cookie jar, so a URL whose ticket another context already
   * spent would load a page with no credential to present.
   */
  async pageURL(): Promise<string> {
    const resp = await fetch(`http://127.0.0.1:${this.bootstrap.port}/pageurl`, {
      // The session token is not a page credential, so it travels as a
      // header — the query slot on the transport's routes belongs to the
      // page ticket.
      headers: { authorization: `Bearer ${this.bootstrap.token}` },
    });
    if (!resp.ok) {
      throw new Error(`harness page url request failed: HTTP ${resp.status}`);
    }
    const url = (await resp.text()).trim();
    if (url === '') throw new Error('harness page url response was empty');
    return url;
  }

  /**
   * Navigates a page to this instance with a ticket of its own. The one
   * navigation helper the suite uses, so no test can reach the app
   * through a spent ticket.
   */
  async open(page: Page, options?: Parameters<Page['goto']>[1]): Promise<void> {
    await page.goto(await this.pageURL(), options);
  }

  async startWatchdog(memoryLimitBytes: number): Promise<void> {
    await this.watchdog.start(memoryLimitBytes);
    this.processIdentity = this.watchdog.processIdentity ?? this.processIdentity;
    this.processTreeProof = this.watchdog.processTreeProof ?? this.processTreeProof;
  }

  private onFrame(raw: string): void {
    const frame = JSON.parse(raw) as {
      type: string;
      id?: string;
      result?: unknown;
      error?: { code: string; message: string };
      channel?: string;
      seq?: number;
      data?: unknown;
      events?: Array<{ channel: string; seq: number; data: unknown }>;
    };
    switch (frame.type) {
      case 'rpc': {
        const pending = this.pending.get(frame.id ?? '');
        if (!pending) return;
        this.pending.delete(frame.id ?? '');
        clearTimeout(pending.timer);
        if (frame.error) {
          pending.reject(new Error(`${frame.error.code}: ${frame.error.message}`));
        } else {
          pending.resolve(frame.result);
        }
        return;
      }
      case 'event':
        this.dispatchEvent({ channel: frame.channel!, seq: frame.seq ?? 0, data: frame.data });
        return;
      case 'batch':
        for (const ev of frame.events ?? []) this.dispatchEvent(ev);
        return;
    }
  }

  private dispatchEvent(ev: WireEvent): void {
    this.eventLog.push(ev);
    if (this.eventLog.length > 10_000) this.eventLog.shift();
    for (const waiter of this.eventWaiters) waiter(ev);
  }

  /**
   * Invoke a backend method by exported name — Harness* RPCs and every
   * bound App method (CreateThread, SendMessage, ...) share the wire.
   */
  async rpc<T = unknown>(method: string, ...params: unknown[]): Promise<T> {
    const ws = this.ws;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      throw new Error(`rpc ${method}: WS is not connected`);
    }
    const id = `e2e-${this.nextId++}`;
    const result = new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        if (!this.pending.delete(id)) return;
        reject(new Error(`rpc ${method} timed out after ${RPC_TIMEOUT_MS}ms`));
      }, RPC_TIMEOUT_MS);
      this.pending.set(id, { resolve, reject, timer });
    });
    try {
      ws.send(JSON.stringify({ type: 'rpc', id, method, params }));
    } catch (error) {
      const pending = this.pending.get(id);
      if (pending) {
        this.pending.delete(id);
        clearTimeout(pending.timer);
        pending.reject(error as Error);
      }
    }
    return (await result) as T;
  }

  /**
   * Wait for an event on a channel (optionally matching a predicate).
   * Checks events already received since the connection opened first,
   * so a fast backend can't win the race against the test — and
   * consumes the matched event, so waiting twice for the same shape
   * observes two distinct occurrences instead of returning the first
   * one again (multi-turn tests depend on this).
   */
  async waitForEvent<T = unknown>(
    channel: string,
    predicate?: (data: T) => boolean,
    timeoutMs = 15_000,
  ): Promise<T> {
    const matches = (ev: WireEvent) =>
      !ev.consumed && ev.channel === channel && (!predicate || predicate(ev.data as T));
    const seen = this.eventLog.find(matches);
    if (seen) {
      seen.consumed = true;
      return seen.data as T;
    }
    return await new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.eventWaiters.delete(waiter);
        const recent = this.eventLog
          .slice(-20)
          .map((e) => e.channel)
          .join(', ');
        reject(
          new Error(
            `timeout waiting for ${channel} event after ${timeoutMs}ms (recent channels: ${recent})`,
          ),
        );
      }, timeoutMs);
      const waiter = (ev: WireEvent) => {
        if (!matches(ev)) return;
        ev.consumed = true;
        clearTimeout(timer);
        this.eventWaiters.delete(waiter);
        resolve(ev.data as T);
      };
      this.eventWaiters.add(waiter);
    });
  }

  /**
   * Count events seen on a channel, consumed or not.
   *
   * The absence half of an event assertion: `waitForEvent` can only prove
   * something happened, and a test that needs to prove something did NOT
   * happen has to read the log directly rather than wait on a timeout. Pair
   * it with a barrier that guarantees the backend is past the point where
   * the event would have fired.
   */
  countEvents<T = unknown>(channel: string, predicate?: (data: T) => boolean): number {
    return this.eventLog.filter(
      (ev) => ev.channel === channel && (!predicate || predicate(ev.data as T)),
    ).length;
  }

  /** Drop remembered events — call after a reset so stale matches can't leak. */
  clearEvents(): void {
    this.eventLog = [];
  }

  /** Wipe all app state (threads, projects, sessions) without a reboot. */
  async reset(): Promise<void> {
    await this.rpc('HarnessReset');
    this.clearEvents();
  }

  /** Gracefully stop the backend while preserving its data directory. */
  async stop(): Promise<boolean> {
    return await this.terminate('SIGTERM');
  }

  /** Kill the backend without cleanup, preserving its data for crash recovery. */
  async crash(): Promise<boolean> {
    return await this.terminate('SIGKILL');
  }

  private async terminate(signal: NodeJS.Signals): Promise<boolean> {
    const state = {
      child: this.child,
      watchdog: this.watchdog,
      memberProof: this.processGroupMemberProof,
      treeProof: this.processTreeProof,
      complete: this.teardownComplete,
      closed: this.closed,
      identity: this.processIdentity,
      socketOpen: () => this.ws?.readyState === WebSocket.OPEN,
      shutdown: () => this.rpc('HarnessShutdown'),
      closeSocket: () => this.ws?.close(),
    };
    const result = await terminateHarness(signal, state);
    this.processGroupMemberProof = state.memberProof;
    this.processTreeProof = state.treeProof;
    this.teardownComplete = state.complete;
    this.closed = state.closed;
    return result;
  }

  /** Terminate the backend and remove the temp data dir it owned. */
  async close(): Promise<void> {
    const stopped = await this.stop();
    if (!stopped) {
      throw new Error(`refusing to remove harness data root while process ${this.child.pid ?? 'unknown'} survives`);
    }
    if (this.removeDataDir) {
      await boundedCleanup(`remove harness data dir ${this.removeDataDir}`, rm(this.removeDataDir, { recursive: true, force: true }));
    }
  }
}
