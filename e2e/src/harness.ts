// TS client for the agent test harness: launches the real backend in
// --harness mode, parses the __AO_HARNESS__ bootstrap line, and speaks
// the transport wire (RPC by method name + event push) over one
// WebSocket. Playwright tests use it for backend setup (seed, scenario
// assignment, replay) and deterministic waits (harness:mock /
// harness:replay events) while the browser exercises the real SPA.
//
// This file is also the reference for driving the harness from any
// other client (Playwright MCP sessions, ad-hoc scripts): everything
// goes through the same bootstrap line + WS wire shown here.

import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { tmpdir } from 'node:os';
import * as path from 'node:path';

const BOOTSTRAP_PREFIX = '__AO_HARNESS__:';

/** The JSON payload of the __AO_HARNESS__ stdout line. */
export interface HarnessBootstrap {
  url: string;
  port: number;
  token: string;
  dataRoot: string;
  dataDir: string;
  homeDir?: string;
  mockProvider: string;
  pid: number;
  version: string;
  startupError?: string;
}

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
}

export interface LaunchOptions {
  /** Backend binary. Default: $AO_HARNESS_BIN, else <repo>/bin/agent-overflow. */
  binary?: string;
  /** ao-mockprovider path. Default: $AO_MOCKPROVIDER, else next to the binary. */
  mockProvider?: string;
  /** Data root. Default: a fresh temp dir, removed on close(). */
  dataDir?: string;
  /** Extra environment (merged over process.env). */
  env?: Record<string, string>;
  /** Boot deadline in ms. Default 30s (first boot migrates the DB). */
  timeoutMs?: number;
}

/** Launch a harness-mode backend and connect to its event wire. */
export async function launchHarness(opts: LaunchOptions = {}): Promise<HarnessApp> {
  const repoRoot = path.resolve(import.meta.dirname, '..', '..');
  const binary =
    opts.binary ?? process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
  const mockProvider = opts.mockProvider ?? process.env.AO_MOCKPROVIDER;
  const ownsDataDir = !opts.dataDir;
  const dataDir = opts.dataDir ?? (await mkdtemp(path.join(tmpdir(), 'ao-harness-')));

  const args = ['--harness', '--data-dir', dataDir];
  if (mockProvider) args.push('--mock-provider', mockProvider);

  const child = spawn(binary, args, {
    env: { ...process.env, ...opts.env },
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  const stderrTail: string[] = [];
  createInterface({ input: child.stderr! }).on('line', (line) => {
    stderrTail.push(line);
    if (stderrTail.length > 200) stderrTail.shift();
    if (process.env.AO_HARNESS_DEBUG) console.error('[backend]', line);
  });

  const bootstrap = await new Promise<HarnessBootstrap>((resolve, reject) => {
    const timeoutMs = opts.timeoutMs ?? 30_000;
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      reject(
        new Error(
          `harness did not print its bootstrap line within ${timeoutMs}ms\n` +
            `binary: ${binary}\nstderr:\n${stderrTail.join('\n')}`,
        ),
      );
    }, timeoutMs);
    createInterface({ input: child.stdout! }).on('line', (line) => {
      const at = line.indexOf(BOOTSTRAP_PREFIX);
      if (at === -1) return;
      clearTimeout(timer);
      try {
        resolve(JSON.parse(line.slice(at + BOOTSTRAP_PREFIX.length)) as HarnessBootstrap);
      } catch (err) {
        reject(new Error(`unparseable harness bootstrap line: ${line} (${err})`));
      }
    });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(
        new Error(
          `harness exited with code ${code} before printing its bootstrap line\n` +
            `binary: ${binary}\nstderr:\n${stderrTail.join('\n')}`,
        ),
      );
    });
  });
  if (bootstrap.startupError) {
    child.kill('SIGTERM');
    throw new Error(`harness backend failed to start: ${bootstrap.startupError}`);
  }

  const app = new HarnessApp(child, bootstrap, ownsDataDir ? dataDir : undefined);
  await app.connect();
  return app;
}

/** A live harness-mode backend plus one WS connection to it. */
export class HarnessApp {
  readonly bootstrap: HarnessBootstrap;
  /** SPA URL including the auth token — pass to page.goto(). */
  readonly url: string;

  private child: ChildProcess;
  private removeDataDir?: string;
  private ws?: WebSocket;
  private nextId = 1;
  private pending = new Map<string, PendingRpc>();
  private eventLog: WireEvent[] = [];
  private eventWaiters = new Set<(ev: WireEvent) => void>();
  private closed = false;

  constructor(child: ChildProcess, bootstrap: HarnessBootstrap, removeDataDir?: string) {
    this.child = child;
    this.bootstrap = bootstrap;
    this.url = bootstrap.url;
    this.removeDataDir = removeDataDir;
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
      for (const p of this.pending.values()) p.reject(err);
      this.pending.clear();
    });
    await new Promise<void>((resolve, reject) => {
      ws.addEventListener('open', () => resolve(), { once: true });
      ws.addEventListener('error', () => reject(new Error('harness WS failed to connect')), {
        once: true,
      });
    });
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
      this.pending.set(id, { resolve, reject });
    });
    ws.send(JSON.stringify({ type: 'rpc', id, method, params }));
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
  async stop(): Promise<void> {
    await this.terminate('SIGTERM');
  }

  /** Kill the backend without cleanup, preserving its data for crash recovery. */
  async crash(): Promise<void> {
    await this.terminate('SIGKILL');
  }

  private async terminate(signal: NodeJS.Signals): Promise<void> {
    this.closed = true;
    this.ws?.close();
    if (this.child.exitCode === null && this.child.signalCode === null) {
      const exited = new Promise<void>((resolve) => this.child.once('exit', () => resolve()));
      this.child.kill(signal);
      const killTimer =
        signal === 'SIGKILL' ? undefined : setTimeout(() => this.child.kill('SIGKILL'), 5_000);
      await exited;
      if (killTimer) clearTimeout(killTimer);
    }
  }

  /** Terminate the backend and remove the temp data dir it owned. */
  async close(): Promise<void> {
    await this.stop();
    if (this.removeDataDir) {
      await rm(this.removeDataDir, { recursive: true, force: true });
    }
  }
}
